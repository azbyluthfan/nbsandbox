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
	"sync/atomic"
	"testing"
	"time"

	"github.com/azbyluthfan/nbsandbox/internal/blob/s3store"
)

// Mediated reads, end to end (make test-connections: the dev stack with
// connections on). The sources live on the gateway's egress network: a
// PostgreSQL database with a read-only role, and an S3 store (RustFS).
// Skipped unless NBSANDBOX_TEST_CONNECTIONS=1.

const (
	srcPG      = "nbsandbox-src-pg"
	srcS3      = "nbsandbox-src-s3"
	srcMySQL   = "nbsandbox-src-mysql"
	srcMSSQL   = "nbsandbox-src-mssql"
	mssqlSA    = "Owner-pw-12345"
	readerPass = "reader-SECRET-pw"
	s3Secret   = "s3-SECRET-key-0123"
)

func needConnections(t *testing.T) {
	t.Helper()
	if os.Getenv("NBSANDBOX_TEST_CONNECTIONS") != "1" {
		t.Skip("set NBSANDBOX_TEST_CONNECTIONS=1 (make test-connections)")
	}
}

// sourcesNet is the Docker network the sources join: the dev stack's
// gateway network, or kind's (Pods resolve its containers' names through
// the node's DNS).
func sourcesNet() string {
	if onKubernetes {
		return "kind"
	}
	return "nbsandbox-dev-egress"
}

// gatewayAddr is where a run would find the gateway: by name on Docker; on
// Kubernetes, run Pods have no DNS, so its Service's address.
func gatewayAddr(t *testing.T) string {
	if onKubernetes {
		return kubectl(t, "-n", "nbsandbox", "get", "svc", "nbsandbox-gateway", "-o", "jsonpath={.spec.clusterIP}")
	}
	return "nbsandbox-gateway"
}

// fetcherAddr is a running fetcher's IP address: what another run would
// need to reach it (by name it couldn't even look it up).
func fetcherAddr(t *testing.T, runID string) string {
	t.Helper()
	var ip string
	if onKubernetes {
		ip = kubectl(t, "-n", "nbsandbox-runs", "get", "pod", "nbsandbox-fetch-"+runID, "-o", "jsonpath={.status.podIP}")
	} else {
		ip = containerIP(t, "nbsandbox-fetch-"+runID)
	}
	if ip == "" {
		t.Fatalf("run %s's fetcher has no address", runID)
	}
	return ip
}

// containerIP is a container's address (its only network's).
func containerIP(t *testing.T, name string) string {
	t.Helper()
	return docker(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
}

// withSQLServer: SQL Server is a test source (on by default). It needs about
// 2 GB of memory, emulated on arm64: NBSANDBOX_TEST_SQLSERVER=0 leaves it out
// where that doesn't fit beside kind.
var withSQLServer = os.Getenv("NBSANDBOX_TEST_SQLSERVER") != "0"

var sourcesOnce sync.Once

// sources starts the data sources once per test binary (and leaves them for
// the next run: make test-connections removes them first).
func sources(t *testing.T) {
	t.Helper()
	sourcesOnce.Do(func() {
		_, _ = runDocker("rm", "-fv", srcPG, srcS3, srcMySQL, srcMSSQL)
		docker(t, "run", "-d", "--name", srcMySQL, "--network", sourcesNet(), "-e", "MYSQL_ROOT_PASSWORD=owner-pw", "-e", "MYSQL_DATABASE=crm",
			"mysql:8.4@sha256:8c233bad33d520a7aae1c1e619c3a9775f4a1ba2d463d692fada04ec7f40be00")
		// SQL Server publishes amd64 images only (emulated on arm64 hosts).
		if withSQLServer {
			docker(t, "run", "-d", "--name", srcMSSQL, "--network", sourcesNet(), "--platform", "linux/amd64",
				"-e", "ACCEPT_EULA=Y", "-e", "MSSQL_SA_PASSWORD="+mssqlSA,
				"mcr.microsoft.com/mssql/server:2022-latest@sha256:4525e3efac9f3e241cb003bb2ff2a82818b502f37e12049d788b1a9fa7e13b7f")
		}
		docker(t, "run", "-d", "--name", srcPG, "--network", sourcesNet(), "-e", "POSTGRES_PASSWORD=owner-pw", "-e", "POSTGRES_DB=crm",
			"postgres:17-alpine@sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac")
		docker(t, "run", "-d", "--name", srcS3, "--network", sourcesNet(), "-p", "127.0.0.1::9000",
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
		// MySQL and SQL Server: the same orders, and a role that may read only them.
		// Over TCP: while it initialises, MySQL runs a temporary server on its
		// socket only, and what it is given is lost when the real one starts.
		retry(t, "source MySQL", 180*time.Second, func() (string, error) {
			return runDocker("exec", srcMySQL, "mysql", "-h127.0.0.1", "--protocol=TCP", "-uroot", "-powner-pw", "crm", "-e", `
				DROP TABLE IF EXISTS orders, payroll;
				DROP USER IF EXISTS 'reader'@'%';
				CREATE TABLE orders (id int, region varchar(10), amount int);
				INSERT INTO orders WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<1000)
				  SELECT n, IF(n % 2 = 0, 'eu', 'us'), n FROM s;
				CREATE TABLE payroll (name varchar(10), salary int);
				CREATE USER 'reader'@'%' IDENTIFIED BY '`+readerPass+`';
				GRANT SELECT ON crm.orders TO 'reader'@'%';`)
		})
		sqlcmd := func(db, q string) (string, error) {
			return runDocker("exec", srcMSSQL, "/opt/mssql-tools18/bin/sqlcmd", "-C", "-b", "-S", "localhost", "-U", "sa", "-P", mssqlSA, "-d", db, "-Q", q)
		}
		if withSQLServer {
			retry(t, "source SQL Server", 180*time.Second, func() (string, error) { return sqlcmd("master", "CREATE DATABASE crm") })
			retry(t, "source SQL Server tables", 30*time.Second, func() (string, error) {
				return sqlcmd("crm", `CREATE TABLE orders (id int, region nvarchar(10), amount int);
				INSERT INTO orders SELECT n, CASE WHEN n % 2 = 0 THEN 'eu' ELSE 'us' END, n
				  FROM (SELECT TOP 1000 ROW_NUMBER() OVER (ORDER BY (SELECT 1)) AS n FROM sys.all_objects a CROSS JOIN sys.all_objects b) s;
				CREATE TABLE payroll (name nvarchar(10), salary int);
				CREATE LOGIN reader WITH PASSWORD = '`+readerPass+`', CHECK_POLICY = OFF;
				CREATE USER reader FOR LOGIN reader;
				GRANT SELECT ON orders TO reader;`)
			})
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

// connSeq keeps connection names apart within a test binary (the time alone
// repeated: two tests made the same name in one run).
var connSeq atomic.Int64

// connection creates one of alice's connections, removed after the test
// (after its space: cleanups run last first).
func connection(t *testing.T, name, typ string, settings map[string]any, secret string) string {
	t.Helper()
	name = fmt.Sprintf("%s-%d", name, connSeq.Add(1)+time.Now().UnixNano()%1_000_000*1000)
	body := map[string]any{"name": name, "type": typ, "settings": settings}
	if secret != "" { // "": a connection without a credential
		body["secret"] = map[string]any{"value": secret}
	}
	r := jsonCall(t, "POST", "/v1/connections", "alice", body, nil)
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
	return readerSpaceWith(t, notebook, nil)
}

// readerSpaceWith is readerSpace with more names (name -> connection ID),
// bound the same in both environments.
func readerSpaceWith(t *testing.T, notebook string, more map[string]string) string {
	t.Helper()
	sources(t)
	crm := connection(t, "crm", "postgresql", pgSettings(), readerPass)
	wrong := connection(t, "crm-wrong", "postgresql", pgSettings(), "not-the-password")
	lake := connection(t, "lake", "s3", s3Settings(), s3Secret)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"read.ipynb": notebook})
	dev, prod := map[string]string{"crm": crm, "lake": lake}, map[string]string{"crm": wrong, "lake": lake}
	for n, id := range more {
		dev[n], prod[n] = id, id
	}
	bind(t, sp, "dev", map[string]map[string]string{"dev": dev, "prod": prod})
	return sp
}

func TestConnectionsRead(t *testing.T) {
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
	needConnections(t)
	sources(t) // the notebook below needs their addresses
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
attempt("direct_db_by_address", lambda: connect("`+containerIP(t, srcPG)+`", 5432))
attempt("direct_s3_by_address", lambda: connect("`+containerIP(t, srcS3)+`", 9000))
attempt("internet", lambda: connect("1.1.1.1", 443))
def through_gateway():
    s = socket.create_connection(("`+gatewayAddr(t)+`", 3128), timeout=3)
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
	seen := found["secret_seen"] != false
	finding(t, "connections-credentials", "a notebook looks for its connections' credentials (environment, run folder)", "not found",
		fmt.Sprintf("found: %v", seen), !seen)
	if seen {
		t.Errorf("the run saw a credential")
	}
	for k, v := range found {
		if k == "secret_seen" {
			continue
		}
		s, _ := v.(string)
		ok := strings.HasPrefix(s, "refused")
		expectRefused(t, "connections-"+k, "a notebook tries "+k, s, ok)
	}
	if s, _ := found["db_role"].(string); !strings.Contains(s, "permission denied") {
		t.Errorf("the database's own role: %q", s)
	}
}

// A source the fetcher can't attach (prod's wrong password) fails the run at
// start, saying why; nothing is left behind.
func TestConnectionsStartFailure(t *testing.T) {
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
	needConnections(t)
	slow := readerSpace(t, codeNotebook(`import nbsandbox, time
nbsandbox.sql("SELECT 1 FROM crm.orders LIMIT 1")
time.sleep(40)`))
	run := startRun(t, "alice", slow, "read.ipynb", nil)
	waitLog(t, "alice", slow, run.ID, "fetcher: serving", 2*time.Minute)
	other := readerSpace(t, codeNotebook(`import nbsandbox, socket
try:
    socket.create_connection(("`+fetcherAddr(t, run.ID)+`", 8815), timeout=3).close()
    nbsandbox.output("reached")
except Exception as e:
    nbsandbox.output("refused: " + str(e))`))
	run2 := startRun(t, "alice", other, "read.ipynb", nil)
	res := runResult(t, "alice", other, run2.ID)
	if res.Status != "succeeded" {
		t.Fatalf("the probing run: %+v", res)
	}
	var s string
	_ = json.Unmarshal(res.Output, &s)
	expectRefused(t, "connections-cross-run", "a run reaches another run's fetcher", s, strings.HasPrefix(s, "refused"))
	call(t, "POST", "/v1/spaces/"+slow+"/runs/"+run.ID+"/cancel", "alice", nil, nil)
	waitRun(t, "alice", slow, run.ID, time.Minute)
}

func getRunEnv(t *testing.T, sp, runID string) string {
	t.Helper()
	var v struct{ Environment string }
	call(t, "GET", "/v1/spaces/"+sp+"/runs/"+runID, "alice", nil, nil).json(t, &v)
	return v.Environment
}

// assertNoReadLeftovers: no fetcher, run network or fetcher Secret outlives
// its run.
func assertNoReadLeftovers(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var left string
		if onKubernetes {
			for _, l := range []string{"nbsandbox.fetcher", "nbsandbox.session.fetcher"} {
				left += kubectl(t, "-n", "nbsandbox-runs", "get", "pods,secrets,networkpolicies", "-l", l, "-o", "name")
			}
		} else {
			left = docker(t, "ps", "-aq", "--filter", "label=nbsandbox.fetcher") + docker(t, "network", "ls", "-q", "--filter", "label=nbsandbox.network") +
				docker(t, "ps", "-aq", "--filter", "label=nbsandbox.session.fetcher") +
				docker(t, "network", "ls", "-q", "--filter", "label=nbsandbox.session.network")
		}
		if left == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("left behind: %q", left)
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

// expectRefused records an isolation finding, and fails the test if what
// was tried wasn't refused.
func expectRefused(t *testing.T, id, attempted, observed string, refused bool) {
	t.Helper()
	finding(t, id, attempted, "refused", observed, refused)
	if !refused {
		t.Errorf("%s: %s was not refused: %q", id, attempted, observed)
	}
}

// retry runs f until it succeeds or the time is up (a source starting).
func retry(t *testing.T, what string, within time.Duration, f func() (string, error)) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		out, err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v\n%s", what, err, out)
		}
		time.Sleep(2 * time.Second)
	}
}

// MySQL and SQL Server, read and joined with PostgreSQL in one query; the
// databases' own roles still hold.
func TestConnectionsMoreDatabases(t *testing.T) {
	needConnections(t)
	sources(t)
	pgc := connection(t, "crm", "postgresql", pgSettings(), readerPass)
	my := connection(t, "shop", "mysql", map[string]any{"host": srcMySQL, "database": "crm", "user": "reader"}, readerPass)
	if !withSQLServer {
		t.Skip("NBSANDBOX_TEST_SQLSERVER=0: no SQL Server source")
	}
	ms := connection(t, "erp", "sqlserver", map[string]any{"host": srcMSSQL, "database": "crm", "user": "reader"}, readerPass)
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"read.ipynb": codeNotebook(`import nbsandbox
def count(q):
    return int(nbsandbox.sql(q).iloc[0, 0])
out = {
    "shop": count("SELECT sum(amount) FROM shop.orders WHERE region = 'eu'"),
    "erp": count("SELECT sum(amount) FROM erp.dbo.orders WHERE region = 'eu'"),
    "joined": count("""SELECT count(*) FROM crm.orders c JOIN shop.orders s USING (id) JOIN erp.dbo.orders e USING (id)
                       WHERE c.region = s.region AND s.region = e.region"""),
}
for name, q in {"shop_role": "SELECT * FROM shop.payroll", "erp_role": "SELECT * FROM erp.dbo.payroll",
                "shop_pass_through": "SELECT * FROM mysql_query('shop', 'SELECT 1')",
                "erp_pass_through": "SELECT mssql_exec('erp', 'DROP TABLE orders')",
                "erp_write": "INSERT INTO erp.dbo.orders VALUES (1, 'eu', 1)"}.items():
    try:
        nbsandbox.sql(q)
        out[name] = "allowed"
    except Exception as e:
        out[name] = "refused: " + str(e)[:200]
nbsandbox.output(out)`)})
	bind(t, sp, "main", map[string]map[string]string{"main": {"crm": pgc, "shop": my, "erp": ms}})
	run := startRun(t, "alice", sp, "read.ipynb", nil)
	res := runResult(t, "alice", sp, run.ID)
	if res.Status != "succeeded" {
		t.Fatalf("run: %+v\n%s", res, call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", "alice", nil, nil).Body)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	// eu: the even ids 2..1000, summed.
	if out["shop"] != float64(250500) || out["erp"] != float64(250500) || out["joined"] != float64(1000) {
		t.Errorf("reads: %v", out)
	}
	for _, k := range []string{"shop_role", "erp_role", "shop_pass_through", "erp_pass_through", "erp_write"} {
		s, _ := out[k].(string)
		expectRefused(t, "connections-"+k, "a notebook tries "+k, s, strings.HasPrefix(s, "refused"))
	}
	assertNoReadLeftovers(t)
}

// An interactive session reads its space's names too, through a fetcher of
// its own that goes when the session ends.
func TestConnectionsSession(t *testing.T) {
	needConnections(t)
	sp := readerSpace(t, codeNotebook("1"))
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/sessions", "alice", map[string]string{"notebook": "read.ipynb", "environment": "dev"}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("open: %d %s", r.Code, r.Body)
	}
	var s struct{ ID, Environment string }
	r.json(t, &s)
	if s.Environment != "dev" {
		t.Fatalf("environment %q", s.Environment)
	}
	ended := false
	end := func() {
		if !ended {
			ended = true
			call(t, "DELETE", "/v1/spaces/"+sp+"/sessions/"+s.ID, "alice", nil, nil)
		}
	}
	t.Cleanup(end)
	k := connectSession(t, "alice", sp, s.ID)
	k.run("import nbsandbox")
	if out, _ := k.run(`print(int(nbsandbox.sql("SELECT count(*) FROM crm.orders").iloc[0, 0]))`); out != "1000\n" {
		t.Fatalf("query: %q", out)
	}
	// Variables stay: a result kept between cells.
	k.run(`df = nbsandbox.sql("SELECT region, sum(amount)::BIGINT AS total FROM lake.csv('sales/2026.csv') GROUP BY 1 ORDER BY 1")`)
	if out, _ := k.run("print(df.total.tolist())"); out != "[17, 5]\n" {
		t.Fatalf("lake: %q", out)
	}
	if out, _ := k.run(`import os, json; print(any(s[::-1] in json.dumps(dict(os.environ)) for s in ["` + reverse(readerPass) + `", "` + reverse(s3Secret) + `"]))`); out != "False\n" {
		t.Fatalf("the session saw a credential: %q", out)
	}
	if r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/sessions", "alice", map[string]string{"notebook": "read.ipynb", "environment": "staging"}, nil); r.Code != http.StatusBadRequest {
		t.Fatalf("unknown environment: %d %s", r.Code, r.Body)
	}
	end()
	assertNoSessionContainer(t, s.ID)
	assertNoReadLeftovers(t)
}

// A published function reads the names its version declared, in the
// environment the call names; a call whose bindings don't match is refused
// before anything runs.
func TestConnectionsFunction(t *testing.T) {
	needConnections(t)
	sp := readerSpace(t, codeNotebook(`import nbsandbox
nbsandbox.output(int(nbsandbox.sql("SELECT count(*) FROM crm.orders WHERE region = 'eu'").iloc[0, 0]))`))
	r := jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/eu_orders/versions", "alice", map[string]any{"notebook": "read.ipynb"}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", r.Code, r.Body)
	}
	var v struct{ Connections map[string]string }
	r.json(t, &v)
	if v.Connections["crm"] != "postgresql" || v.Connections["lake"] != "s3" {
		t.Fatalf("declared: %v", v.Connections)
	}
	r = jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/eu_orders/invoke?wait=50s", "alice", map[string]any{"environment": "dev"}, nil)
	var res callerResult
	_ = json.Unmarshal(r.Body, &res)
	if res.Status != "succeeded" || string(res.Output) != "500" {
		t.Fatalf("invoke: %d %s", r.Code, r.Body)
	}
	// The space now binds crm to an object store: the version's crm is gone.
	lake := connection(t, "lake2", "s3", s3Settings(), s3Secret)
	bind(t, sp, "main", map[string]map[string]string{"main": {"crm": lake, "lake": lake}})
	// Cleanups run last first: unbind before lake2's is deleted (a bound
	// connection can't be), or it is left behind.
	t.Cleanup(func() {
		jsonCall(t, "PUT", "/v1/spaces/"+sp+"/connections", "alice", map[string]any{"environments": map[string]map[string]string{}}, nil)
	})
	r = jsonCall(t, "POST", "/v1/spaces/"+sp+"/functions/eu_orders/invoke", "alice", map[string]any{}, nil)
	if r.Code != http.StatusBadRequest || !strings.Contains(string(r.Body), "crm (postgresql, bound to a s3 connection)") {
		t.Fatalf("mismatched bindings: %d %s", r.Code, r.Body)
	}
	assertNoReadLeftovers(t)
}

// With secrets in Vault (make test-k8s-vault): a connection can reference
// a secret the platform team keeps, under a prefix the operator allows; the
// fetcher reads it itself. Managed secrets are covered by every other test
// there.
func TestConnectionsVaultReference(t *testing.T) {
	needConnections(t)
	if os.Getenv("NBSANDBOX_TEST_VAULT") != "1" {
		t.Skip("set NBSANDBOX_TEST_VAULT=1 (make test-k8s-vault)")
	}
	sources(t)
	for ref, want := range map[string]int{"teams/elsewhere#password": http.StatusBadRequest, "other/crm#password": http.StatusBadRequest} {
		r := jsonCall(t, "POST", "/v1/connections", "alice", map[string]any{"name": fmt.Sprintf("bad-%d", time.Now().UnixNano()%1_000_000),
			"type": "postgresql", "settings": pgSettings(), "secret": map[string]any{"ref": ref}}, nil)
		if r.Code != want {
			t.Errorf("reference %s: %d %s", ref, r.Code, r.Body)
		}
	}
	name := fmt.Sprintf("crm-ref-%d", time.Now().UnixNano()%1_000_000)
	r := jsonCall(t, "POST", "/v1/connections", "alice", map[string]any{"name": name, "type": "postgresql", "settings": pgSettings(),
		"secret": map[string]any{"ref": "teams/data/crm#password"}}, nil)
	if r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	var c struct{ ID string }
	r.json(t, &c)
	t.Cleanup(func() { call(t, "DELETE", "/v1/connections/"+c.ID, "alice", nil, nil) })
	sp := space(t, "alice")
	upload(t, "alice", sp, "", map[string]string{"read.ipynb": codeNotebook(`import nbsandbox
nbsandbox.output(int(nbsandbox.sql("SELECT count(*) FROM crm.orders").iloc[0, 0]))`)})
	bind(t, sp, "main", map[string]map[string]string{"main": {"crm": c.ID}})
	run := startRun(t, "alice", sp, "read.ipynb", nil)
	res := runResult(t, "alice", sp, run.ID)
	if res.Status != "succeeded" || string(res.Output) != "1000" {
		t.Fatalf("run: %+v %s", res, call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", "alice", nil, nil).Body)
	}
	assertNoReadLeftovers(t)
}
