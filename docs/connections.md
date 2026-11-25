# Reading data where it lives: connections

A notebook can read a database or an object store without the data being copied into nbsandbox, and without seeing a host name or a password. The user registers a **connection** once (where the data is, and its credential), and a space gives it a short **name**. The notebook then queries the name:

```python
import nbsandbox

orders = nbsandbox.sql("SELECT region, sum(amount) AS total FROM crm.orders GROUP BY 1")   # a pandas DataFrame
```

Runs still have no network. Each reading run gets a **fetcher**: a second container that holds the run's credentials, runs the query with DuckDB, and streams the result to the run as Arrow. The fetcher reaches its sources only through nbsandbox's **egress gateway**, and only the endpoints of that run's connections. The design is in [plan-data-access.md](plan-data-access.md).

This is off unless the operator turns it on ([below](#for-operators)).

**What works today:**
- PostgreSQL databases, and S3-compatible object storage (AWS S3, MinIO, Ceph, RustFS), with CSV and Parquet files.
- Runs on the Docker backend.

**Not yet:** MySQL and SQL Server, Azure Blob storage, HTTP APIs (connections of those types can be created and bound, but runs can't read them yet); interactive sessions and published functions (a function will read with its caller's connections); the Kubernetes backend (a run whose space binds names is refused there for now); connections in the web UI.

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
| `postgresql` | `host`, `port` (5432), `database`, `user` | the password |
| `s3` | `endpoint` (`host[:port]`, no scheme), `bucket`, `region` (us-east-1), `access_key_id`, `use_ssl` (true), `url_style` (`vhost`, or `path` for MinIO, Ceph, RustFS) | the secret access key |

**Give nbsandbox a read-only user.** The database's own permissions are what really limits a query: nbsandbox refuses writes and more, but a role that can only `SELECT` the tables a notebook needs is the guarantee.

A connection may point only where the operator allows (`connections.allowed`); anything else is refused when you create it, and when a run starts.

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

**What a query may not do** (it fails with `nbsandbox.QueryError`, saying why): anything but one `SELECT`; send SQL to a source as is (`postgres_query`, …); read files or URLs other than its names' buckets; change settings or load extensions.

### 4. Run, in an environment

```sh
curl -u alice:… -X POST https://nbsandbox.example/v1/spaces/$SPACE/runs -H 'Content-Type: application/json' \
  -d '{"notebook": "report.ipynb", "environment": "prod"}'
```

Without `environment`, the space's default. The run records which environment it used (`"environment"` on the run), and which connection each name meant when it was submitted.

**If the fetcher can't start** (a wrong password, a database down), the run fails at start, `platform_error`, with the fetcher's message; its log shows the fetcher's output, prefixed `[fetcher]`. A connection deleted while a run waited fails it the same way.

**Live data:** a run reads the source as it is at that moment. For results you can reproduce, read a snapshot (a dated table, a versioned Parquet folder).

## What a run can and can't reach

| | |
|---|---|
| The credentials | Never in the run: only the fetcher has them. The run gets a token for its own fetcher. |
| The sources directly | No: the run's network is private to it and its fetcher (and the gateway, which needs the fetcher's own token). |
| Another run's fetcher | No: each reading run has its own network. |
| The internet | No. |
| The fetcher's files and settings | No: it reads only its run's buckets, and its configuration is locked after start. |
| The gateway | Only to the endpoints in the run's token, never loopback or link-local (cloud metadata) addresses, whatever the token says. |

The integration tests check each row (`make test-connections`).

## For operators

On the Docker backend:

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

**What each reading run costs:** a second container (the fetcher, with the run's memory and CPU limits) and a Docker network, both removed when the run ends; leftovers after a crash are removed at the next start. The run starts once the fetcher has attached its sources.

**Audit:** creating, changing and deleting connections and bindings are audited; the gateway logs each tunnel and request it allows or refuses (run, endpoint, bytes, duration).
