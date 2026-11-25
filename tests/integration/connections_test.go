// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob/s3store"
)

// Mediated reads, end to end (make test-connections: the dev stack with
// connections on). The sources live on the gateway's egress network: a
// PostgreSQL database with a read-only role, and an S3 store (RustFS).
// Skipped unless NBSANDBOX_TEST_CONNECTIONS=1.

const (
	egressNet  = "nbsandbox-dev-egress"
	srcPG      = "nbsandbox-src-pg"
	srcS3      = "nbsandbox-src-s3"
	readerPass = "reader-SECRET-pw"
	s3Secret   = "s3-SECRET-key-0123"
)

func needConnections(t *testing.T) {
	t.Helper()
	if os.Getenv("NBSANDBOX_TEST_CONNECTIONS") != "1" {
		t.Skip("set NBSANDBOX_TEST_CONNECTIONS=1 (make test-connections)")
	}
}

var sourcesOnce sync.Once

// sources starts the data sources once per test binary (and leaves them for
// the next run: make test-connections removes them first).
func sources(t *testing.T) {
	t.Helper()
	sourcesOnce.Do(func() {
		_, _ = runDocker("rm", "-f", srcPG, srcS3)
		docker(t, "run", "-d", "--name", srcPG, "--network", egressNet, "-e", "POSTGRES_PASSWORD=owner-pw", "-e", "POSTGRES_DB=crm",
			"postgres:17-alpine@sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac")
		docker(t, "run", "-d", "--name", srcS3, "--network", egressNet, "-p", "127.0.0.1::9000",
			"-e", "RUSTFS_ACCESS_KEY=reader", "-e", "RUSTFS_SECRET_KEY="+s3Secret,
			"rustfs/rustfs:1.0.4@sha256:ed9210d6411d77ffceb70d94b964a3cb7d225983368bc25bebe01e995293dcc2")
		// The database: orders the role may read, a table it may not.
		deadline := time.Now().Add(60 * time.Second)
		for {
			out, err := runDocker("exec", srcPG, "psql", "-U", "postgres", "-d", "crm", "-v", "ON_ERROR_STOP=1", "-c", `
				CREATE TABLE orders (id int, region text, amount numeric);
				INSERT INTO orders SELECT g, CASE WHEN g % 2 = 0 THEN 'eu' ELSE 'us' END, g FROM generate_series(1, 1000) g;
				CREATE TABLE payroll (name text, salary int);
				CREATE ROLE reader LOGIN PASSWORD '`+readerPass+`';
				GRANT USAGE ON SCHEMA public TO reader;
				GRANT SELECT ON orders TO reader;`)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("source database: %v\n%s", err, out)
			}
			time.Sleep(time.Second)
		}
		// The object store: a CSV in the bound bucket, another bucket beside it.
		port := strings.TrimPrefix(docker(t, "port", srcS3, "9000/tcp"), "127.0.0.1:")
		for bucket, obj := range map[string][2]string{
			"lake":    {"sales/2026.csv", "region,amount\neu,10\nus,5\neu,7\n"},
			"private": {"salaries.csv", "name,salary\nbob,100\n"},
		} {
			var st *s3store.Store
			var err error
			for i := 0; i < 30; i++ {
				st, err = s3store.New(context.Background(), s3store.Config{Endpoint: "127.0.0.1:" + port, Bucket: bucket,
					AccessKey: "reader", SecretKey: s3Secret, CreateBucket: true})
				if err == nil {
					break
				}
				time.Sleep(time.Second)
			}
			if err != nil {
				t.Fatalf("s3 bucket %s: %v", bucket, err)
			}
			if err := st.Put(context.Background(), obj[0], strings.NewReader(obj[1]), int64(len(obj[1]))); err != nil {
				t.Fatalf("s3 object: %v", err)
			}
		}
	})
}

func runDocker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// connection creates one of alice's connections, removed after the test
// (after its space: cleanups run last first).
func connection(t *testing.T, name, typ string, settings map[string]any, secret string) string {
	t.Helper()
	name = fmt.Sprintf("%s-%d", name, time.Now().UnixNano()%1_000_000)
	r := jsonCall(t, "POST", "/v1/connections", "alice", map[string]any{"name": name, "type": typ, "settings": settings,
		"secret": map[string]any{"value": secret}}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("create connection: %d %s", r.Code, r.Body)
	}
	var c struct{ ID string }
	r.json(t, &c)
	t.Cleanup(func() { call(t, "DELETE", "/v1/connections/"+c.ID, "alice", nil, nil) })
	return c.ID
}

func bind(t *testing.T, sp string, def string, envs map[string]map[string]string) {
	t.Helper()
	r := jsonCall(t, "PUT", "/v1/spaces/"+sp+"/connections", "alice", map[string]any{"default_environment": def, "environments": envs}, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", r.Code, r.Body)
	}
}

func pgSettings() map[string]any {
	return map[string]any{"host": srcPG, "database": "crm", "user": "reader"}
}

func s3Settings() map[string]any {
	return map[string]any{"endpoint": srcS3 + ":9000", "bucket": "lake", "access_key_id": "reader", "use_ssl": false, "url_style": "path"}
}

// readerSpace is a space binding crm and lake (dev), and crm with a wrong
// password (prod), with the notebook given.
func readerSpace(t *testing.T, notebook string) string {
	t.Helper()
	sources(t)
	crm := connection(t, "crm", "postgresql", pgSettings(), readerPass)
	wrong := connection(t, "crm-wrong", "postgresql", pgSettings(), "not-the-password")
	lake := connection(t, "lake", "s3", s3Settings(), s3Secret)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"read.ipynb": notebook})
	bind(t, sp, "dev", map[string]map[string]string{"dev": {"crm": crm, "lake": lake}, "prod": {"crm": wrong, "lake": lake}})
	return sp
}

func TestConnectionsRead(t *testing.T) {
	needCompose(t)
	needConnections(t)
	sp := readerSpace(t, codeNotebook(`import nbsandbox
by_region = nbsandbox.sql("SELECT region, count(*) AS n, sum(amount)::BIGINT AS total FROM crm.orders GROUP BY 1 ORDER BY 1")
lake = nbsandbox.sql("SELECT region, sum(amount)::BIGINT AS total FROM lake.csv('sales/2026.csv') GROUP BY 1 ORDER BY 1")
joined = nbsandbox.sql("""SELECT o.region, count(*) AS n, any_value(l.total)::BIGINT AS lake_total
  FROM crm.orders o JOIN (SELECT region, sum(amount) AS total FROM lake.csv('sales/2026.csv') GROUP BY 1) l USING (region)
  GROUP BY 1 ORDER BY 1""")
rows = sum(b.num_rows for b in nbsandbox.stream("SELECT * FROM crm.orders"))
nbsandbox.output({"by_region": by_region, "lake": lake, "joined": joined, "streamed": rows,
                  "connections": sorted(c["name"] for c in nbsandbox.connections())})`))
	run := startRun(t, "alice", sp, "read.ipynb", nil)
	res := runResult(t, "alice", sp, run.ID)
	if res.Status != "succeeded" {
		t.Fatalf("run: %+v\n%s", res, call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", "alice", nil, nil).Body)
	}
	var out struct {
		ByRegion []struct {
			Region   string
			N, Total int64
		} `json:"by_region"`
		Lake []struct {
			Region string
			Total  int64
		} `json:"lake"`
		Joined []struct {
			Region    string
			N         int64
			LakeTotal int64 `json:"lake_total"`
		} `json:"joined"`
		Streamed    int64    `json:"streamed"`
		Connections []string `json:"connections"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.ByRegion) != 2 || out.ByRegion[0].Region != "eu" || out.ByRegion[0].N != 500 || out.ByRegion[0].Total != 250500 {
		t.Errorf("crm: %+v", out.ByRegion)
	}
	if len(out.Lake) != 2 || out.Lake[0].Total != 17 || out.Lake[1].Total != 5 {
		t.Errorf("lake: %+v", out.Lake)
	}
	if len(out.Joined) != 2 || out.Joined[0].N != 500 || out.Joined[0].LakeTotal != 17 {
		t.Errorf("join across sources: %+v\n%s", out.Joined, res.Output)
	}
	if out.Streamed != 1000 || strings.Join(out.Connections, ",") != "crm,lake" {
		t.Errorf("streamed %d, connections %v", out.Streamed, out.Connections)
	}
	if g := getRunEnv(t, sp, run.ID); g != "dev" {
		t.Errorf("environment: %q", g)
	}
	assertNoReadLeftovers(t)
}

// What a notebook can't do, whatever it tries.
func TestConnectionsIsolation(t *testing.T) {
	needCompose(t)
	needConnections(t)
	sp := readerSpace(t, codeNotebook(`import nbsandbox, os, socket, json
found = {}
def attempt(name, f):
    try:
        f()
        found[name] = "allowed"
    except Exception as e:
        found[name] = "refused: " + str(e)[:200]

# The credentials are nowhere the notebook can look.
blob = json.dumps(dict(os.environ))
for root, _, files in os.walk("/run"):
    for f in files:
        try:
            blob += open(os.path.join(root, f), errors="ignore").read()
        except OSError:
            pass
# Reversed here, so this notebook's own snapshot (in /run) doesn't hold them.
found["secret_seen"] = any(s[::-1] in blob for s in ["`+reverse(readerPass)+`", "`+reverse(s3Secret)+`"])

def connect(host, port):
    socket.create_connection((host, port), timeout=3).close()
attempt("direct_db", lambda: connect("`+srcPG+`", 5432))
attempt("direct_s3", lambda: connect("`+srcS3+`", 9000))
attempt("internet", lambda: connect("1.1.1.1", 443))
def through_gateway():
    s = socket.create_connection(("nbsandbox-gateway", 3128), timeout=3)
    tok = os.environ["NBSANDBOX_FETCHER_TOKEN"]
    s.sendall(f"CONNECT `+srcPG+`:5432 HTTP/1.1\r\nHost: x\r\nProxy-Authorization: Bearer {tok}\r\n\r\n".encode())
    status = s.recv(200).split(b"\r\n")[0].decode()
    if " 200 " in status:
        return
    raise RuntimeError(status)
attempt("gateway_with_runner_token", through_gateway)

for name, q in {
    "pass_through": "SELECT * FROM postgres_query('crm', 'SELECT 1')",
    "write": "INSERT INTO crm.orders VALUES (1, 'eu', 1)",
    "two_statements": "SELECT 1; SELECT 2",
    "local_file": "SELECT * FROM read_csv('/etc/passwd')",
    "other_bucket": "SELECT * FROM read_csv('s3://private/salaries.csv')",
    "dotdot": "SELECT * FROM lake.csv('../private/salaries.csv')",
    "settings": "SELECT * FROM duckdb_settings()",
    "db_role": "SELECT * FROM crm.payroll",
}.items():
    attempt(name, lambda q=q: nbsandbox.sql(q))
nbsandbox.output(found)`))
	run := startRun(t, "alice", sp, "read.ipynb", nil)
	res := runResult(t, "alice", sp, run.ID)
	if res.Status != "succeeded" {
		t.Fatalf("run: %+v", res)
	}
	var found map[string]any
	if err := json.Unmarshal(res.Output, &found); err != nil {
		t.Fatal(err)
	}
	if found["secret_seen"] != false {
		t.Errorf("the run saw a credential")
	}
	for k, v := range found {
		if k == "secret_seen" {
			continue
		}
		s, _ := v.(string)
		ok := strings.HasPrefix(s, "refused")
		finding(t, "connections-"+k, "a notebook tries "+k, "refused", s, ok)
	}
	if s, _ := found["db_role"].(string); !strings.Contains(s, "permission denied") {
		t.Errorf("the database's own role: %q", s)
	}
}

// A source the fetcher can't attach (prod's wrong password) fails the run at
// start, saying why; nothing is left behind.
func TestConnectionsStartFailure(t *testing.T) {
	needCompose(t)
	needConnections(t)
	sp := readerSpace(t, codeNotebook(`import nbsandbox
nbsandbox.output(nbsandbox.connections())`))
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]any{"notebook": "read.ipynb", "environment": "prod"}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", r.Code, r.Body)
	}
	var run Run
	r.json(t, &run)
	got := waitRun(t, "alice", sp, run.ID, 3*time.Minute)
	if got.Status != "platform_error" || !strings.Contains(got.Message, "password authentication failed") ||
		strings.Contains(got.Message, "not-the-password") {
		t.Fatalf("run: %+v", got)
	}
	if r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/runs", "alice", map[string]any{"notebook": "read.ipynb", "environment": "staging"}, nil); r.Code != http.StatusBadRequest {
		t.Fatalf("unknown environment: %d %s", r.Code, r.Body)
	}
	assertNoReadLeftovers(t)
}

// Two reading runs at once: neither reaches the other's fetcher.
func TestConnectionsRunsSeparated(t *testing.T) {
	needCompose(t)
	needConnections(t)
	slow := readerSpace(t, codeNotebook(`import nbsandbox, time
nbsandbox.sql("SELECT 1 FROM crm.orders LIMIT 1")
time.sleep(40)`))
	run := startRun(t, "alice", slow, "read.ipynb", nil)
	deadline := time.Now().Add(2 * time.Minute)
	for getRun(t, "alice", slow, run.ID).Status != "running" {
		if time.Now().After(deadline) {
			t.Fatal("the first run didn't start")
		}
		time.Sleep(500 * time.Millisecond)
	}
	other := readerSpace(t, codeNotebook(`import nbsandbox, socket
try:
    socket.create_connection(("nbsandbox-fetch-`+run.ID+`", 8815), timeout=3).close()
    nbsandbox.output("reached")
except Exception as e:
    nbsandbox.output("refused: " + str(e))`))
	run2 := startRun(t, "alice", other, "read.ipynb", nil)
	res := runResult(t, "alice", other, run2.ID)
	var s string
	_ = json.Unmarshal(res.Output, &s)
	finding(t, "connections-cross-run", "a run reaches another run's fetcher", "refused", s, strings.HasPrefix(s, "refused"))
	call(t, "POST", "/v1/spaces/"+slow+"/runs/"+run.ID+"/cancel", "alice", nil, nil)
	waitRun(t, "alice", slow, run.ID, time.Minute)
}

func getRunEnv(t *testing.T, sp, runID string) string {
	t.Helper()
	var v struct{ Environment string }
	call(t, "GET", "/v1/spaces/"+sp+"/runs/"+runID, "alice", nil, nil).json(t, &v)
	return v.Environment
}

// assertNoReadLeftovers: no fetcher container or run network outlives its run.
func assertNoReadLeftovers(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		f := docker(t, "ps", "-aq", "--filter", "label=nbsandbox.fetcher")
		n := docker(t, "network", "ls", "-q", "--filter", "label=nbsandbox.network")
		if f == "" && n == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("left behind: fetchers %q, networks %q", f, n)
		}
		time.Sleep(time.Second)
	}
}

func reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
