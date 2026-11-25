# Reading data where it lives: connections

A notebook can read a database or an object store without the data being copied into nbsandbox, and without seeing a host name or a password. The user registers a **connection** once (where the data is, and its credential), and a space gives it a short **name**. The notebook then queries the name:

```python
import nbsandbox

orders = nbsandbox.sql("SELECT region, sum(amount) AS total FROM crm.orders GROUP BY 1")   # a pandas DataFrame
```

Runs still have no network. Each reading run gets a **fetcher**: a second container that holds the run's credentials, runs the query with DuckDB, and streams the result to the run as Arrow. The fetcher reaches its sources only through nbsandbox's **egress gateway**, and only the endpoints of that run's connections. The design is in [plan-data-access.md](plan-data-access.md).

This is off unless the operator turns it on ([below](#for-operators)).

**What works today:**
- PostgreSQL, MySQL and SQL Server databases, and S3-compatible object storage (AWS S3, MinIO, Ceph, RustFS), with CSV and Parquet files.
- Runs, interactive sessions and published functions, on Docker and on Kubernetes.

**Not yet:** Azure Blob storage and HTTP APIs (connections of those types can be created and bound, but nothing reads them yet).

**In the web UI:** **Connections** on the spaces page lists yours, to add, change or delete (the secret field is write-only); each space's page has a **Connections** section, a name per row and an environment per column; and where a space has several environments, a picker next to **Run** chooses the one runs, sessions and function calls read in.

## For users

### 1. Create a connection

A connection is yours: no one else can see or use it. Its secret is stored by nbsandbox's secret provider and never returned.

```sh
curl -u alice:… -X POST https://nbsandbox.example/v1/connections -H 'Content-Type: application/json' -d '{
  "name": "crm-prod",
  "type": "postgresql",
  "settings": {"host": "pg.internal", "database": "crm", "user": "nbsandbox_reader"},
  "secret": {"value": "…"}
}'
```

| Type | Settings | The secret |
|---|---|---|
| `postgresql` | `host`, `port` (5432), `database`, `user`, `tls` (`prefer`), `ca` | the password |
| `mysql` | `host`, `port` (3306), `database`, `user`, `tls` (`prefer`), `ca` | the password |
| `sqlserver` | `host`, `port` (1433), `database`, `user` (a SQL login), `tls` (`prefer`; not `verify`) | the password |
| `s3` | `endpoint` (`host[:port]`, no scheme), `bucket`, `region` (us-east-1), `access_key_id`, `use_ssl` (true), `url_style` (`vhost`, or `path` for MinIO, Ceph, RustFS), `ca` | the secret access key |

**Give nbsandbox a read-only user.** The database's own permissions are what really limits a query: nbsandbox refuses writes and more, but a role that can only `SELECT` the tables a notebook needs is the guarantee.

A connection may point only where the operator allows (`connections.allowed`); anything else is refused when you create it, and when a run starts.

**Encryption.** A database's `tls` says how nbsandbox talks to it:

| `tls` | |
|---|---|
| `off` | never encrypted |
| `prefer` (the default) | encrypted if the server offers it, but its certificate isn't checked |
| `require` | always encrypted, certificate not checked |
| `verify` | encrypted, and the certificate must come from a trusted authority and name the `host` you gave |

Only `verify` stops someone on the network between nbsandbox and the database from reading the data, or MySQL's password. Use it for any database you reach over a network you don't control. `verify` trusts the system's authorities (public ones), or, if you give `ca` (PEM certificates), those instead: your organisation's CA, or the bundle your cloud publishes for its databases. A refused certificate fails the run when it starts, with the driver's reason.

- MySQL with `verify` needs `host` to be a name, not an address.
- SQL Server connections can't be verified yet: DuckDB's SQL Server extension accepts any certificate. Use `require`, and reach SQL Server over a network you trust.
- Object storage with `use_ssl` (the default) always checks the certificate against the system's authorities. Give `ca` for a store with a private one (an internal MinIO).

### 2. Bind names in a space

A space binds names to connections, in one or more **environments**: the same names in each, so the same notebook reads development or production data without edits.

```sh
curl -u alice:… -X PUT https://nbsandbox.example/v1/spaces/$SPACE/connections -H 'Content-Type: application/json' -d '{
  "default_environment": "dev",
  "environments": {
    "dev":  {"crm": "crm-dev",  "lake": "lake-sample"},
    "prod": {"crm": "crm-prod", "lake": "company-lake"}
  }
}'
```

Names are lowercase SQL identifiers (`crm`, `sales_lake`). A connection a space binds can't be deleted until it's unbound.

### 3. Query in a notebook

```python
import nbsandbox

df = nbsandbox.sql("SELECT * FROM crm.orders WHERE created_at >= '2026-01-01'")       # a DataFrame
for batch in nbsandbox.stream("SELECT * FROM crm.events"):                           # pyarrow RecordBatches
    ...                                                                              # process, then let go
nbsandbox.connections()                                                              # [{"name": "crm", "type": "postgresql"}, ...]
```

- **Databases** are ordinary tables: `crm.orders`, or `crm.public.orders` with the schema.
- **Object storage:** each name has two table functions, with paths inside the bucket (globs allowed):
  ```python
  nbsandbox.sql("SELECT region, sum(amount) FROM lake.parquet('sales/2026/*.parquet') GROUP BY 1")
  nbsandbox.sql("SELECT * FROM lake.csv('exports/latest.csv')")
  ```
- **One query can join several names:** `crm.customers JOIN lake.parquet('invoices/*.parquet') USING (customer_id)`. Filters and the columns you select are pushed to PostgreSQL; Parquet files are read only for the columns and row groups needed.
- **Big results:** `stream()` hands over a few MB at a time, so a run can process far more than its memory. Write what you keep to the run's output folder (`NBSANDBOX_OUTPUT_DIR`), as Parquet for example.
- The SQL is DuckDB's dialect.

**What a query may not do** (it fails with `nbsandbox.QueryError`, saying why): anything but one `SELECT`; send SQL to a source as is (`postgres_query`, …); read files or URLs other than its names' buckets; put a path in `FROM` (`FROM 's3://…'`: use `lake.parquet('…')`); compute a table function's arguments (write paths out: `lake.csv('a.csv')`, not `lake.csv(x || '.csv')`); change settings or load extensions.

### 4. Run, in an environment

```sh
curl -u alice:… -X POST https://nbsandbox.example/v1/spaces/$SPACE/runs -H 'Content-Type: application/json' \
  -d '{"notebook": "report.ipynb", "environment": "prod"}'
```

Without `environment`, the space's default. The run records which environment it used (`"environment"` on the run), and which connection each name meant when it was submitted.

**If the fetcher can't start** (a wrong password, a database down), the run fails at start, `platform_error`, with the fetcher's message; its log shows the fetcher's output, prefixed `[fetcher]`. A connection deleted while a run waited fails it the same way.

**Live data:** a run reads the source as it is at that moment. For results you can reproduce, read a snapshot (a dated table, a versioned Parquet folder).

### 5. In an interactive session

A session reads the same names, from its cells ([sessions.md](sessions.md)): open it with `"environment"` (the space's default otherwise). Its fetcher starts with it and goes when it ends; an open session keeps the environment it was opened in.

```sh
curl -u alice:… -X POST https://nbsandbox.example/v1/spaces/$SPACE/sessions -H 'Content-Type: application/json' \
  -d '{"notebook": "explore.ipynb", "environment": "dev"}'
```

### 6. In a published function

A version records the names its space bound when it was published, and their types: `"connections": {"crm": "postgresql"}` on the version. A call names the environment (`"environment"` on `…/invoke`, the space's default otherwise), which must bind every one of those names to a connection of the same type; otherwise the call is refused (`400`) before anything runs, saying which names are missing. The run reads only the names its version declared.

Functions are called by their space's owner today, so they read the space's own bindings. With shared spaces, each caller's own connections will stand behind the same names.

## What a run can and can't reach

| | |
|---|---|
| The credentials | Never in the run: only the fetcher has them. The run gets a token for its own fetcher. |
| The sources directly | No: the run's network is private to it and its fetcher (and the gateway, which needs the fetcher's own token). |
| Another run's fetcher | No: each reading run has its own network (Docker), or its own network policies (Kubernetes). |
| The internet | No. |
| The fetcher's files and settings | No: it reads only its run's buckets, and its configuration is locked after start. |
| The gateway | Only to the endpoints in the run's token, never loopback or link-local (cloud metadata) addresses, whatever the token says. |

The integration tests check each row (`make test-connections`).

## When something fails

| What fails | What a notebook sees |
|---|---|
| A source stops | Its queries raise `nbsandbox.QueryError` with the source's error; they work again once it is back. |
| The gateway restarts | Queries work again once it is back: its tokens are signed, not stored. |
| The fetcher stops (out of memory, its Pod deleted) | Every query raises `QueryError`: "the fetcher can't be reached". A run carries on without its data; a session needs starting again. A fetcher whose network vanishes without closing its connections (a lost node) fails a waiting query within about six and a half minutes. |
| nbsandboxd restarts | A session reconnects and reads through the same fetcher; a run ends as interrupted. Either way, a fetcher and its network don't outlive their run or session. |

The integration tests check each row (`TestConnectionsFetcherDies`, `TestConnectionsSessionFailures`, `TestConnectionsRunInterrupted`).

## For operators

### On Kubernetes (Helm)

```yaml
connections:
  enabled: true
  allowed: ["*.db.internal", "10.20.0.0/16", "s3.eu-west-1.amazonaws.com"]
  secretsProvider: kubernetes       # Secrets in the runs namespace
  gateway:
    keySecret: nbsandbox-gateway    # a Secret with key "key": openssl rand -base64 32
    replicas: 2
  fetcherImage: ghcr.io/azbyluthfan/nbsandbox-fetcher:<version>
```

- **Secrets:** a connection's secret is a Secret in the runs namespace: one nbsandbox writes (`nbsandbox-conn-<id>`, labelled), or one you manage and reference as `name#key` (GitOps, Sealed Secrets, External Secrets Operator). The fetcher Pod **mounts** it: nbsandboxd never reads it. With the `kubernetes` provider, the API processes get a token and a Role limited to Secrets in the runs namespace; otherwise they still have none.
- **The gateway** is a Deployment and a Service in the release namespace, with a NetworkPolicy that lets only fetchers in. Allow its egress to your sources in whatever egress controls the cluster has (it needs DNS too).
- **Each reading run** gets a fetcher Pod in the runs namespace (its own limits, reserved like a run's: `connections.fetcherMemory` and `fetcherCpus`; the run's runtime class; no API credentials, no DNS: the worker resolves the gateway's address), a Secret with its configuration and tokens, and two NetworkPolicies: its runner may reach its fetcher, and nothing else may. A chart policy lets fetchers reach the gateway's port. The workers' Role gains Secrets and NetworkPolicies in the runs namespace.
- The namespace's deny-all and the workers' check that it's enforced are unchanged: the network plugin must enforce NetworkPolicy (Cilium, Calico, Azure's).

### Vault or OpenBao

Secrets can live in Vault or OpenBao (KV version 2) instead of Kubernetes Secrets, on the Kubernetes backend. nbsandboxd writes the secrets users enter under its own path and checks that references exist; it never reads a value. Each fetcher logs in itself and reads only its run's paths.

```yaml
connections:
  secretsProvider: vault
  vault:
    address: https://vault.vault.svc:8200
    mount: secret                     # the KV v2 mount
    prefix: nbsandbox/connections     # managed secrets; nothing else writes here
    authMount: kubernetes
    role: nbsandbox                   # the API processes' role
    fetcherRole: nbsandbox-fetcher    # the fetchers' role
    references: [teams/data]          # prefixes connections may reference (path#key)
    caConfigMap: ""                   # a ConfigMap with ca.crt, for a private CA
```

In Vault, enable the Kubernetes auth method and create two roles and policies:

| Role | Bound to | Policy |
|---|---|---|
| `nbsandbox` | service account `nbsandbox-api` in the release namespace | `create`, `update` on `secret/data/nbsandbox/connections/*`; `read`, `delete` on `secret/metadata/nbsandbox/connections/*`; `read` on `secret/metadata/<each reference prefix>/*` |
| `nbsandbox-fetcher` | service account `nbsandbox-fetcher` in the runs namespace, audience `vault` | `read` on `secret/data/nbsandbox/connections/*` and `secret/data/<each reference prefix>/*` |

- **What a fetcher may read** is that role's policy, the same for every fetcher of the installation; what it does read is its own run's paths. It logs in with a token of its service account made for Vault's audience only (the Kubernetes API refuses it), and revokes its Vault token once it has read them.
- **Fetchers reach Vault through the gateway,** like their sources: the gateway must be able to reach Vault's address. Vault's address needn't be in `connections.allowed`.
- **A connection references** a secret as `path#key` under the KV mount (`teams/data/crm#password`), under one of `references`.
- `deploy/kubernetes/kind/openbao-setup.sh` sets up OpenBao this way for the tests (`make test-k8s-vault`).

### On Docker

1. **Settings** (`settings.yaml`, or the environment):
   ```yaml
   connections:
     enabled: true
     allowed: ["*.db.internal", "10.20.0.0/16", "s3.eu-west-1.amazonaws.com"]   # where connections may point
     secrets:
       provider: builtin
       master_key: …          # NBSANDBOX_CONNECTIONS_MASTER_KEY: openssl rand -base64 32
     gateway:
       address: nbsandbox-gateway:3128
       key: …                 # NBSANDBOX_GATEWAY_KEY: openssl rand -base64 32, the gateway's too
     fetcher_image: ghcr.io/azbyluthfan/nbsandbox-fetcher:<version>
   ```
   The built-in secret store keeps secrets encrypted in nbsandbox's database with the master key: losing the key loses every stored secret. On Kubernetes, use the `kubernetes` provider (Secrets in the runs namespace).
2. **The gateway:** `docker compose --profile connections up -d` in `deploy/` starts it (`nbsandboxd gateway`, the nbsandboxd image) with the same key, alone on its own network. Its container name is the host of `connections.gateway.address`. It must reach your sources; nothing else of nbsandbox's is on its network.
3. **The Docker socket proxy** must allow network calls: `GET /networks`, `POST /networks/create`, `POST /networks/{id}/connect` and `/disconnect`, `DELETE /networks/{id}`. The Compose files in `deploy/` do.
4. **The fetcher image** must be on the Docker host, like the runner image.

**What each reading run costs:** a second container or Pod (the fetcher, with its own limits: `connections.fetcher_memory`, 1 GiB, and `fetcher_cpus`, 1; DuckDB spills to disk past three quarters of the memory) and a Docker network or two NetworkPolicies and a Secret, all removed when the run ends; leftovers after a crash are removed at the next start. The run starts once the fetcher has attached its sources.

**Audit:** creating, changing and deleting connections and bindings are audited; the gateway logs each tunnel and request it allows or refuses (run, endpoint, bytes, duration).
