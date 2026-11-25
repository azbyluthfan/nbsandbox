// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sources that speak TLS, with certificates from a test authority: a
// database that accepts nothing else (PostgreSQL, MySQL) and an object store
// over HTTPS. Each also answers to a second name its certificate doesn't
// carry (…-alias): what a man in the middle would look like to the fetcher.

const (
	srcTLSPG    = "nbsandbox-src-tls-pg"
	srcTLSMySQL = "nbsandbox-src-tls-mysql"
	srcTLSS3    = "nbsandbox-src-tls-s3"
)

var (
	tlsOnce sync.Once
	// testCA signed the sources' certificate; otherCA signed nothing here.
	testCA, otherCA string
)

// testAuthority is a CA: its certificate (PEM) and a signer for leaves.
type testAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newAuthority(t *testing.T, name string) testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return testAuthority{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// leaf is a server certificate and its key (PEM) for names.
func (a testAuthority) leaf(t *testing.T, names ...string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
}

// tlsSources starts the TLS sources once per test binary. Certificates go in
// by environment (no files shared with the Docker host); each container
// writes its own.
func tlsSources(t *testing.T) {
	t.Helper()
	tlsOnce.Do(func() {
		ca, other := newAuthority(t, "nbsandbox test CA"), newAuthority(t, "another CA")
		testCA, otherCA = ca.pem, other.pem
		cert, key := ca.leaf(t, srcTLSPG, srcTLSMySQL, srcTLSS3)
		env := []string{"-e", "TLS_CERT=" + cert, "-e", "TLS_KEY=" + key, "-e", "TLS_CA=" + ca.pem}
		const write = `mkdir -p /tmp/tls && printf '%s' "$TLS_CERT" > /tmp/tls/cert.pem && printf '%s' "$TLS_KEY" > /tmp/tls/key.pem && printf '%s' "$TLS_CA" > /tmp/tls/ca.pem && chmod 600 /tmp/tls/key.pem`
		_, _ = runDocker("rm", "-fv", srcTLSPG, srcTLSMySQL, srcTLSS3)

		// PostgreSQL: TLS connections only (hostssl), with a password.
		docker(t, append(append([]string{"run", "-d", "--name", srcTLSPG, "--network", sourcesNet(), "--network-alias", srcTLSPG + "-alias",
			"-e", "POSTGRES_PASSWORD=owner-pw", "-e", "POSTGRES_DB=crm"}, env...),
			"--entrypoint", "sh", "postgres:17-alpine@sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac", "-c",
			write+` && printf 'local all all trust\nhostssl all all all scram-sha-256\n' > /tmp/tls/hba && chown -R postgres /tmp/tls &&
			exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/tmp/tls/cert.pem -c ssl_key_file=/tmp/tls/key.pem -c hba_file=/tmp/tls/hba`)...)
		// MySQL: require_secure_transport; small, as a second MySQL beside kind
		// otherwise starves its node.
		docker(t, append(append([]string{"run", "-d", "--name", srcTLSMySQL, "--network", sourcesNet(), "--network-alias", srcTLSMySQL + "-alias",
			"-e", "MYSQL_ROOT_PASSWORD=owner-pw", "-e", "MYSQL_DATABASE=crm"}, env...),
			"--entrypoint", "sh", "mysql:8.4@sha256:8c233bad33d520a7aae1c1e619c3a9775f4a1ba2d463d692fada04ec7f40be00", "-c",
			write+` && chown -R mysql /tmp/tls && exec docker-entrypoint.sh mysqld --ssl-ca=/tmp/tls/ca.pem --ssl-cert=/tmp/tls/cert.pem --ssl-key=/tmp/tls/key.pem --require-secure-transport=ON --performance-schema=OFF --innodb-buffer-pool-size=32M`)...)
		// The object store: HTTPS only, serving lake/sales.csv path-style
		// (signatures aren't checked: what's tested is the channel). Python
		// from the fetcher's image, which the tests build anyway.
		docker(t, append(append([]string{"run", "-d", "--name", srcTLSS3, "--network", sourcesNet()}, env...),
			"--entrypoint", "sh", "nbsandbox-fetcher:dev", "-c", write+` && mkdir -p /tmp/www/lake && printf 'region,amount\neu,10\nus,5\n' > /tmp/www/lake/sales.csv &&
			cd /tmp/www && exec python -c "
import http.server, ssl
h = http.server.ThreadingHTTPServer(('0.0.0.0', 8443), http.server.SimpleHTTPRequestHandler)
c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); c.load_cert_chain('/tmp/tls/cert.pem', '/tmp/tls/key.pem')
h.socket = c.wrap_socket(h.socket, server_side=True); h.serve_forever()"`)...)

		retry(t, "TLS source PostgreSQL", 90*time.Second, func() (string, error) {
			return runDocker("exec", srcTLSPG, "psql", "-U", "postgres", "-d", "crm", "-v", "ON_ERROR_STOP=1", "-c", `
				DROP TABLE IF EXISTS orders; DROP ROLE IF EXISTS reader;
				CREATE TABLE orders (id int, region text, amount numeric);
				INSERT INTO orders SELECT g, CASE WHEN g % 2 = 0 THEN 'eu' ELSE 'us' END, g FROM generate_series(1, 100) g;
				CREATE ROLE reader LOGIN PASSWORD '`+readerPass+`';
				GRANT SELECT ON orders TO reader;`)
		})
		retry(t, "TLS source MySQL", 180*time.Second, func() (string, error) {
			return runDocker("exec", srcTLSMySQL, "mysql", "-h127.0.0.1", "--protocol=TCP", "-uroot", "-powner-pw", "crm", "-e", `
				DROP TABLE IF EXISTS orders;
				DROP USER IF EXISTS 'reader'@'%';
				CREATE TABLE orders (id int, region varchar(10), amount int);
				INSERT INTO orders WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n<100)
				  SELECT n, IF(n % 2 = 0, 'eu', 'us'), n FROM s;
				CREATE USER 'reader'@'%' IDENTIFIED BY '`+readerPass+`';
				GRANT SELECT ON crm.orders TO 'reader'@'%';`)
		})
		retry(t, "TLS source S3", 30*time.Second, func() (string, error) {
			return runDocker("exec", srcTLSS3, "python", "-c",
				"import ssl,urllib.request; print(urllib.request.urlopen('https://localhost:8443/lake/sales.csv', context=ssl._create_unverified_context()).read())")
		})
	})
}

func tlsPG(host, mode, ca string) map[string]any {
	s := map[string]any{"host": host, "database": "crm", "user": "reader", "tls": mode}
	if ca != "" {
		s["ca"] = ca
	}
	return s
}

func tlsMySQL(host, ca string) map[string]any {
	s := map[string]any{"host": host, "database": "crm", "user": "reader", "tls": "verify"}
	if ca != "" {
		s["ca"] = ca
	}
	return s
}

func tlsS3(ca string) map[string]any {
	s := map[string]any{"endpoint": srcTLSS3 + ":8443", "bucket": "lake", "access_key_id": "reader", "url_style": "path", "use_ssl": true}
	if ca != "" {
		s["ca"] = ca
	}
	return s
}

// Reading sources over TLS: checked certificates pass; a certificate from
// another authority, or for another name, stops the fetcher before a query.
func TestConnectionsTLS(t *testing.T) {
	needConnections(t)
	sources(t)
	tlsSources(t)
	conn := func(t *testing.T, name, typ string, s map[string]any) string {
		return connection(t, name, typ, s, readerPass)
	}
	pgVerify := conn(t, "pg-verify", "postgresql", tlsPG(srcTLSPG, "verify", testCA))
	pgRequire := conn(t, "pg-require", "postgresql", tlsPG(srcTLSPG, "require", ""))
	myVerify := conn(t, "my-verify", "mysql", tlsMySQL(srcTLSMySQL, testCA))
	lake := conn(t, "lake-https", "s3", tlsS3(testCA))

	notebooks := map[string]string{
		"read.ipynb": codeNotebook(`import nbsandbox
q = "SELECT region, count(*) AS n FROM {}.orders GROUP BY 1 ORDER BY 1"
nbsandbox.output({"pg": nbsandbox.sql(q.format("pg")), "pgreq": nbsandbox.sql(q.format("pgreq")),
                  "my": nbsandbox.sql(q.format("my")),
                  "lake": nbsandbox.sql("SELECT region, sum(amount)::BIGINT AS total FROM lake.csv('sales.csv') GROUP BY 1 ORDER BY 1")})`),
		"lake.ipynb": codeNotebook(`import nbsandbox
try:
    nbsandbox.sql("SELECT * FROM lake.csv('sales.csv')")
    nbsandbox.output("read")
except Exception as e:
    nbsandbox.output(str(e))`),
		"names.ipynb": codeNotebook(`import nbsandbox
nbsandbox.output(nbsandbox.connections())`),
	}
	// A space binding names (one environment, "dev"), and a run of a notebook in it.
	spaceWith := func(t *testing.T, names map[string]string) string {
		sp := space(t, "alice")
		upload(t, "alice", sp, "", notebooks)
		bind(t, sp, "dev", map[string]map[string]string{"dev": names})
		return sp
	}
	start := func(sp, nb string) Run {
		t.Helper()
		return startRun(t, "alice", sp, nb, nil)
	}
	sp := spaceWith(t, map[string]string{"pg": pgVerify, "pgreq": pgRequire, "my": myVerify, "lake": lake})

	t.Run("verified", func(t *testing.T) {
		run := start(sp, "read.ipynb")
		res := runResult(t, "alice", sp, run.ID)
		if res.Status != "succeeded" {
			t.Fatalf("run: %+v %+v\n%s", res, res.Error, call(t, "GET", "/v1/spaces/"+sp+"/runs/"+run.ID+"/log", "alice", nil, nil).Body)
		}
		var out map[string][]struct {
			Region   string
			N, Total int64
		}
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"pg", "pgreq", "my"} {
			if r := out[k]; len(r) != 2 || r[0].Region != "eu" || r[0].N != 50 {
				t.Errorf("%s: %+v", k, r)
			}
		}
		if r := out["lake"]; len(r) != 2 || r[0].Total != 10 || r[1].Total != 5 {
			t.Errorf("lake: %+v", r)
		}
	})

	// The fetcher refuses the source when it starts: the run fails with the
	// driver's reason, and never the password.
	for env, c := range map[string]struct {
		name, typ string
		settings  map[string]any
		want      string
		attempted string
	}{
		"pg_system":  {"pg", "postgresql", tlsPG(srcTLSPG, "verify", ""), "certificate verify failed", "PostgreSQL, tls verify, a certificate from an authority the system doesn't trust"},
		"pg_otherca": {"pg", "postgresql", tlsPG(srcTLSPG, "verify", otherCA), "certificate verify failed", "PostgreSQL, tls verify, a certificate from another authority than the connection's ca"},
		"pg_name":    {"pg", "postgresql", tlsPG(srcTLSPG+"-alias", "verify", testCA), "does not match host name", "PostgreSQL, tls verify, reached by a name its certificate doesn't carry"},
		"pg_off":     {"pg", "postgresql", tlsPG(srcTLSPG, "off", ""), "no encryption", "PostgreSQL, tls off, to a server that accepts only TLS"},
		"my_name":    {"my", "mysql", tlsMySQL(srcTLSMySQL+"-alias", testCA), "Hostname verification failed", "MySQL, tls verify, reached by a name its certificate doesn't carry"},
		"my_otherca": {"my", "mysql", tlsMySQL(srcTLSMySQL, otherCA), "certificate", "MySQL, tls verify, a certificate from another authority than the connection's ca"},
	} {
		t.Run(env, func(t *testing.T) {
			bad := spaceWith(t, map[string]string{c.name: conn(t, env, c.typ, c.settings)})
			got := waitRun(t, "alice", bad, start(bad, "names.ipynb").ID, 3*time.Minute)
			if strings.Contains(got.Message, readerPass) {
				t.Errorf("the password is in the run's message: %q", got.Message)
			}
			expectRefused(t, "connections-tls_"+env, c.attempted, got.Status+": "+got.Message,
				got.Status == "platform_error" && strings.Contains(got.Message, c.want))
		})
	}

	// S3 connects when a query reads: the query fails, the run goes on.
	t.Run("lake_noca", func(t *testing.T) {
		bad := spaceWith(t, map[string]string{"lake": conn(t, "lake_noca", "s3", tlsS3(""))})
		res := runResult(t, "alice", bad, start(bad, "lake.ipynb").ID)
		var msg string
		_ = json.Unmarshal(res.Output, &msg)
		if res.Status != "succeeded" {
			t.Fatalf("run: %+v", res)
		}
		expectRefused(t, "connections-tls_lake_noca", "S3 over HTTPS, a certificate from an authority the system doesn't trust", msg,
			msg != "read" && strings.Contains(strings.ToLower(msg), "certificate"))
	})
	assertNoReadLeftovers(t)
}
