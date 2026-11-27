# Plan: data outside nbsandbox

**Status (2026-11-25):** phase 1 of the [order of work](#order-of-work) is built, in milestones M0 to M5: connections, secret providers (Kubernetes Secrets, Vault/OpenBao, a built-in store for development), names and environments, mediated reads (PostgreSQL, MySQL, SQL Server, S3-compatible storage) for runs, sessions and functions, the egress gateway, on Docker and Kubernetes, the web UI, and the isolation rows. User and operator documentation: [connections.md](connections.md). Phase 2, HTTP APIs through the fetcher, is designed ([below](#http-apis-through-the-fetcher), 2026-11-27). The later phases are on the [ROADMAP](../ROADMAP.md).

## Today

A run sees only its space's files (copied to `/space` when it starts), its package set and `/tmp`. It has **no network**: no database, no object store, no API. Data must be uploaded into a space first, within its quota (500 MiB by default), and is copied into every run. A run is one container with fixed limits (1 CPU, 2 GiB by default).

That works for small data that lives in nbsandbox. It doesn't work for the usual case: **the user's data lives somewhere else**, in a database or an object store, and it can be large.

## Goal and constraints

Let notebooks read data where it lives, at any size, without weakening what nbsandbox guarantees. Four constraints shape every choice below:

1. **Self-hosted.** Runs on the customer's own Kubernetes: AKS and on-premises clusters. Only open-source, self-hostable components; no vendor platform (Databricks, Snowflake and the like) is required.
2. **A complete, clean way to read data.** Nothing that assumes the customer already runs something else (a Spark cluster, a warehouse): nbsandbox brings what's needed.
3. **No bulk copies.** Large data is never copied into a run or a worker to make it readable: disk, I/O and latency don't scale.
4. **Distributed-ready.** The same design must work when a run spreads over several machines. Nothing may assume one container.

**Kubernetes only.** Data access is built for the Kubernetes backend. The Docker backend gets it for development (a built-in secret store, Docker networks), not for production.

## Design

### Connections

A **connection** is a data source a user can read: **a type, an endpoint and how to authenticate**.

| Type | Examples | Read as |
|---|---|---|
| PostgreSQL | any version still supported | tables, SQL |
| MySQL | MySQL, MariaDB | tables, SQL |
| SQL Server | on-premises, Azure SQL | tables, SQL |
| Object storage | S3-compatible (MinIO, Ceph, RustFS, cloud), Azure Blob / ADLS | Parquet and CSV files; Iceberg tables |
| HTTP | the customer's own APIs, including streaming (SSE) | by the notebook's own code, through the run's fetcher, which adds the credential ([HTTP APIs](#http-apis-through-the-fetcher)) |

- **Connections belong to a user**, defined once, at user level, and used by any number of their spaces. Rotating a password happens in one place. A connection's page lists where it is used, and a connection in use can't be deleted until it is detached.
- **The operator limits where connections may point**: the address ranges and domains allowed (`settings.yaml`), so users can't aim nbsandbox at arbitrary internal systems.

### Secrets

A connection's credential (a password, a key, a token, an OAuth client secret) is kept in a **secret provider**. The values never pass through nbsandboxd's API responses, logs or database.

| Provider | For | Notes |
|---|---|---|
| **Kubernetes Secrets** | production | covers Sealed Secrets and External Secrets Operator too: both produce ordinary Kubernetes Secrets |
| **Vault / OpenBao** | production | KV secrets |
| Built-in store | development only (the Docker backend) | encrypted in nbsandbox's PostgreSQL with a master key the operator provides; not offered for production |

Each connection's secret is either:
- **managed:** the user enters it in nbsandbox's UI or API (write-only: it is never shown again), and nbsandbox writes it into **a place only nbsandbox writes to**: Secrets it creates and labels in the **runs namespace** (a Pod can mount only its own namespace's Secrets, and the fetcher runs there; run Pods have no API access, no service account token, and network policy blocks the API server, so only a fetcher ever mounts one), or its own Vault path. This is the self-service way, as Airflow's or Grafana's own stores are; or
- **referenced:** the connection points to an existing Kubernetes Secret (in the runs namespace, for the same reason) or Vault path, created outside nbsandbox (GitOps, Sealed Secrets, the platform team). Read-only, as Airflow's secrets backends or Databricks' Key Vault-backed scopes work.

**nbsandboxd never handles a secret's value when a run starts.** It creates the run's fetcher Pod with *references* to exactly the secrets that run needs, and the kubelet mounts them. With Vault, the fetcher logs in itself (Vault's Kubernetes auth, with a token of its own service account for Vault's audience) and reads only its run's paths; what it *may* read is the fetcher role's policy, the same for every fetcher of an installation.

### Spaces, names and environments

A notebook never names a connection, a host or a secret, only a **name** its space defines:

```python
orders = nbsandbox.sql("SELECT region, sum(amount) FROM crm.orders WHERE year = 2026 GROUP BY 1")
```

- **A space binds names to connections:** `crm` → the user's `crm-prod`, `lake` → `company-lake`. A space can bind several, and one query can join across them (`crm.customers JOIN billing.invoices`).
- **Environments:** a space can have named sets of bindings, such as `dev` and `prod`, with the same names in each:

  ```
  space "forecast"     environment dev (default)     environment prod
                         crm  → crm-dev                crm  → crm-prod
                         lake → lake-sample            lake → company-lake
  ```

  A run, session or function call picks the environment (the space's default otherwise), so the same notebook reads dev or prod data without edits. Every name must be bound in every environment, checked when the environment is saved.
- `nbsandbox.connections()` lists the names a run can use and their type, never hosts or secrets. Table lists come with the editor, which will complete `crm.` with table names.

**How a name is resolved, all on nbsandbox's side:**

```
run → its space → the chosen environment → the name → a connection → its secret (reference)
```

Every run records its environment and which connection each name meant.

### Functions and shared spaces: the caller's connections

- **A function call resolves names with the caller's own connections.** The publisher declares the names a function needs and what they must be (`crm: postgresql, tables orders, customers`). A caller without a matching connection gets a clear error before anything runs. Each run records whose connections it used. Nothing is pinned to the publisher's credentials, so nothing breaks when the publisher leaves.
- **Shared spaces** (no organisations; [plan-shared-spaces.md](plan-shared-spaces.md)): each member runs the space's notebooks with **their own connections** under the space's names, which the space's owners declare.

### Two modes: mediated and injected

| | Mediated (the default) | Injected |
|---|---|---|
| For | SQL and files: the database and object-storage types | anything the notebook's own code must call: HTTP and SSE APIs, a storage layer without a built-in type, a vendor SDK |
| Who connects | the run's **fetcher** | the **notebook's code** |
| The secret | stays in the fetcher; never in the run | given to the run (environment or file), **redacted** from logs, outputs and results |
| The run's network | only to its own fetcher | also to that connection's endpoint, through the egress gateway |
| What the notebook gets | Arrow data: `nbsandbox.sql(...)`, `nbsandbox.stream(...)` | `nbsandbox.connection("events_api")`: endpoint, headers or token, for `requests`, an SSE client or an SDK |

Injected mode is the user's own secret in the user's own code. The risk is accidental exposure, which redaction and the single allowed endpoint limit. Mediated mode is preferred wherever it fits. Injected mode replaces the ROADMAP's earlier "network access to registered endpoints" item.

**HTTP APIs don't need injected mode** (decided 2026-11-27): the notebook's own code makes the requests, but through the run's fetcher, which adds the credential ([below](#http-apis-through-the-fetcher)). The secret stays out of the run, as in mediated mode. Injected mode waits until a client needs the raw credential itself (an SDK that signs requests or runs an OAuth flow of its own, a database driver in the notebook).

### HTTP APIs through the fetcher

The notebook calls its API with any HTTP client; the run's fetcher forwards each request to the connection's URL with the credential added, through the egress gateway.

```
notebook code ── http://<fetcher>:8816/events/orders?since=… ──► fetcher ── CONNECT ──► gateway ──► https://api.example.com/v2/orders?since=…
   (no secret; the run's token)                                     (adds the credential; TLS to the API)
```

**In the notebook:**

```python
api = nbsandbox.http("events")                 # a requests.Session for the connection
orders = api.get("orders", params={"since": "2026-11-01"}).json()

for line in api.get("stream", stream=True).iter_lines():   # SSE, streamed as it arrives
    ...

c = nbsandbox.connection("events")             # for other clients and SDKs:
c.url                                          #   the base URL to call (the fetcher's)
c.headers                                      #   the header carrying the run's token
c.token                                        #   the run's token, as an SDK's API key
client = SomeSDK(base_url=c.url, api_key=c.token)
```

- **The run's token stands in for the credential.** The fetcher accepts it in `X-Nbsandbox-Token` or in the connection's own auth header (so an SDK that takes a base URL and an API key works unchanged), checks it, strips both, and sets the real credential. A request without the run's token is refused.
- **What a connection allows:** its URL's scheme, host and port, and paths under its path (`https://api.example.com/v2/` allows `/v2/…`, not `/admin`), checked after percent-decoding and resolving `..`. Any method. As with a database's role, **the credential's own permissions in the API are the real limit**; the docs and the connection form say so.
- **Authentication,** a connection's `auth`:
  - `header` (the default; today's `http` connections): the secret in a header, `auth_header` and `auth_prefix` as now (`Authorization: Bearer …`, `X-API-Key: …`). The secret stays optional, as before: without one, no header is set;
  - `basic`: a `user` setting, the password as the secret;
  - `oauth2`: OAuth 2.0 client credentials. Settings `token_url`, `client_id`, `scope` and `audience` (optional), the client secret as the secret. The fetcher gets an access token, keeps it in memory until a minute before it expires, and gets a new one once on a `401`. The token URL's host joins the run's gateway token and must be allowed by the operator, like the API's;
  - `none`: an API that needs no credential, reached only through the gateway like any other.
- **Requests and responses stream both ways,** unbuffered: large uploads and downloads, SSE and chunked responses work, and the fetcher's memory doesn't grow with them. Hop-by-hop and `Proxy-*` headers are dropped, `Host` is the API's. WebSocket upgrades are refused for now.
- **Redirects are not followed:** a redirect under the connection's URL is rewritten to the fetcher's address, any other is passed back as is (the run can't reach it), and the credential never follows one.
- **TLS:** the fetcher checks the API's certificate against the system's authorities, plus `ca` (a private authority) if set. Plain `http://` URLs are allowed, as today, and the connection form warns about them.
- **The fetcher** serves HTTP on a second port, 8816, beside Flight's 8815, with its standard library (a threaded server and `http.client` over its existing gateway tunnel); no new package. At most 32 requests at a time per run.
- **Logs:** the fetcher logs each request (name, method, path without the query string, status, bytes each way, duration) to the run's fetcher log; the gateway logs each tunnel as for databases. Query strings and bodies are never logged.
- **Unchanged:** the run reaches only its fetcher (on Kubernetes, the per-run NetworkPolicy gains port 8816); nbsandboxd resolves names, secrets and the gateway token as for databases; runs, sessions and function calls work alike; `nbsandbox.connections()` lists HTTP names with their type.
- **Contract,** so any image can use it: `NBSANDBOX_FETCHER_HTTP` (the base address, `http://<fetcher>:8816/`) and the run's existing `NBSANDBOX_FETCHER_TOKEN`; a connection's base URL is `NBSANDBOX_FETCHER_HTTP` plus its name and `/`.

### A run with several runners

A run becomes **one orchestrator runner, N worker runners (none today) and one fetcher**, on **a private network of its own**:

```
run 42 — one private network, removed with the run
 ┌──────────────┐   tasks, results   ┌──────────┐   ┌──────────┐
 │ orchestrator │◄──────────────────►│ worker 1 │ … │ worker N │    the notebook's code; no secrets
 └──────┬───────┘                    └────┬─────┘   └────┬─────┘    (mediated mode)
        │       Arrow (Flight), per split │              │
        └───────────────┬─────────────────┴──────────────┘
                 ┌──────▼──────┐
                 │   fetcher   │   only run 42's secrets; DuckDB; no notebook code
                 └──────┬──────┘
                        ▼
                 egress gateway ──► only run 42's connection endpoints
```

- **One trust domain per run:** the orchestrator and workers run the same user's code with the same rights, so data shared among them crosses no boundary. The boundary is between runs.
- **The private network:** a run's runners reach each other and their fetcher, nothing else (not other runs, nbsandboxd, the host or the internet). In injected mode, also the egress gateway.
- **Each runner and fetcher gets a token bound to its run**, so a worker of run 42 can't fetch for run 43.
- **Elastic start:** workers take tasks (splits) as they become free, so a run doesn't need all its workers to begin. It starts once its orchestrator, its fetcher and a **minimum fraction of its requested workers** can be placed (for example `0.5`: half of them), and the rest join as capacity appears. `1.0` means all of them, for engines that need a fixed number of peers. nbsandbox's own admission decides this, extending today's run queue; no external scheduler.
  - **Rounded up:** `0.5` of 3 workers means 2.
  - **A minimum that can never fit is refused when the run is submitted** (larger than the cluster or the per-run cap), with a clear error; a queued run waits at most a set time.
  - **The operator sets the default fraction, `0.5`;** a run or function can override it within the operator's limits.
- **Limits** apply per runner and to the run as a whole. Sizes per run, the ROADMAP's "resource tiers", become a choice of runner sizes and counts.
- **Everything ends with the run:** runners, fetcher, network and intermediate data.
- Interactive sessions get the same shape: a session's kernel is its orchestrator, with a fetcher of its own.

### The fetcher

- **Its own image, one Pod per run**, with its own limits (`connections.fetcher_memory`, `fetcher_cpus`). It never runs notebook code. It holds only that run's resolved credentials. **Written in Python** (`duckdb` and `pyarrow`, whose Arrow export and Flight are C++), with **DuckDB pinned to the 1.5 series** ([spike results](#spike-results-m0-2026-11-25)).
- **DuckDB** as the engine, locked down in three layers, since its SQL comes from the notebook (DuckDB's own docs say SQL from untrusted sources needs sandboxing; the fetcher Pod is that sandbox):
  - **a gate:** only a single `SELECT`, and no pass-through functions unless the connection allows them, checked with DuckDB's own parser before anything runs;
  - **DuckDB's settings:** only the run's own sources allowed, then external access off, extensions off, configuration locked;
  - **the database role:** read-only and least-privilege in the source.
- **Credentials as DuckDB secrets** (its secret manager, attached by name), never in connection strings, which SQL can list.
- **Reads in place:** Parquet and Iceberg statistics skip files and row groups, and only the needed columns are read.
- **Databases:** PostgreSQL and MySQL through DuckDB's own core extensions; SQL Server through the community `mssql` extension (its native protocol, with `ATTACH` and pushdown), validated against a real SQL Server in CI. ODBC only as a fallback for databases nothing else covers: DuckDB's ODBC extension sends query text only, with no pushdown. The same connectors can write later.
- **Simple reads by default:** a source receives only generated scans (these columns, these filters), and joins and aggregation run in DuckDB, so nbsandbox controls exactly what each source sees. Passing more SQL through to a source can be enabled per connection, relying on its database role.
- **Serves Arrow over Flight** (plain Flight, the query in the ticket; there is no Python Flight SQL server) to the run's runners. A query's result can be split, one stream per worker, which is the basis of distributed mode.
- **The source's own permissions are the real boundary.** Each connection's database user should be **read-only and least-privilege in the database**. nbsandbox's table lists help users; they are not the security guarantee, and the docs and the connection form say so.

### The fetcher contract

How a notebook talks to its fetcher, so that any image can, not only nbsandbox's runner (custom images are on the ROADMAP):
- **Plain Arrow Flight,** an open standard with clients in Python (`pyarrow`), R (`arrow`), Julia, Go, Java and more.
- **Two environment variables** in the run: `NBSANDBOX_FETCHER` (the fetcher's address, a `grpc://` URL) and `NBSANDBOX_FETCHER_TOKEN` (the run's token, sent as `authorization: Bearer …` on every call).
- **A query is a `DoGet` whose ticket is the SQL text,** answered with Arrow record batches. The action `connections` lists the run's names and their type.
- **The `nbsandbox` Python helper** (`sql`, `stream`, `connections`) is a thin layer over this, needing only `pyarrow` (in nbsandbox's runner image; a custom Python image installs `nbsandbox` and `pyarrow`).

### Object storage in SQL

Each object-storage name is a schema with two table functions, paths relative to the connection's bucket, which the notebook never sees:

```python
nbsandbox.sql("SELECT region, sum(amount) FROM lake.parquet('sales/2026/*.parquet') GROUP BY 1")
nbsandbox.sql("SELECT * FROM lake.csv('exports/latest.csv')")
```

DuckDB's allow-list keeps reads inside the bucket. Databases are ordinary tables: `crm.orders`, or `crm.public.orders`.

### Egress on AKS and on-premises

Kubernetes NetworkPolicy works on IP addresses. DNS-name rules exist only in some engines (Cilium; on AKS, an add-on), so they aren't portable. Instead:
- **Runs and fetchers may reach only the egress gateway** (plus their run's private network): plain NetworkPolicy, enforced by every engine AKS offers and by Calico or Cilium on-premises. nbsandbox already refuses to run where network policies aren't enforced.
- **The egress gateway** is our own small proxy in Go: the nbsandboxd binary's `gateway` command, **one per cluster**, run with several replicas. It **resolves names itself** and allows only the run's connection endpoints: HTTPS by name (`CONNECT`), databases as a TCP tunnel per connection. Each connection is logged.
- **A run's allowed endpoints reach the gateway in a signed token.** When a run starts, nbsandboxd issues it a token naming the run, its allowed `host:port` list and an expiry. The gateway only checks the signature: it keeps no state and makes no lookup per connection, and the token dies with the run.
- **Injected mode:** notebook database clients can't talk to a proxy, so the runner starts a small forwarder inside the run, and `nbsandbox.connection("crm")` gives the notebook a local address (`127.0.0.1:15432`) that tunnels through the gateway. HTTP clients use `HTTPS_PROXY`.
- It can later move to one gateway per node, or to Envoy, if traffic calls for it, without changing the rest of the design.
- **The same on Docker:** the gateway is a container of the stack (`nbsandboxd gateway`), alone on its own network, and nbsandboxd connects it to each run's private network. The fetcher has no way out of its own there either, so both backends have one egress path and one set of isolation tests.
- No dependency on a particular CNI, no DNS inside runs, and endpoints whose addresses change (cloud storage) work.

### Results and writes

- **Results:** streaming first (`nbsandbox.stream`: Arrow batches a run processes and discards), small results as DataFrames, and big outputs written as **Parquet artifacts** in nbsandbox's object storage (what runs already do with their output folder).
- **Writes to external sources** come later, as their own phase: atomic Iceberg commits first, database inserts after (where a failed run can leave a partial write, as with Spark's JDBC writes). With a separate write-capable connection, so read-only stays the default.

### Distributed mode

The design already has its parts: workers, splits and the run's private network. Distributed mode adds:
- **Splits:** a query's Flight endpoints are divided among workers, each fetching its own share in parallel.
- **DuckDB on each worker** for partial aggregates, combined by the orchestrator. This covers scans, filters and aggregations.
- **Another engine only when needed:**
  - **large joins or group-bys across workers**, where data must be redistributed between machines: DataFusion with Ballista;
  - **Python-heavy distributed work** (custom Python per row, model inference, images and documents): Daft on Ray, or Ray Data;
  - **customers who require the Spark API:** Spark, the heaviest, only on demand.

## Security

New guarantees, each with an isolation-report row, as for runs and sessions:
- **Secrets stay out of runs** in mediated mode and for HTTP APIs: absent from the run's environment, files, logs, outputs and API responses. Injected secrets are redacted wherever nbsandbox writes text.
- **Names are bounded:** a run can use only its space's names in the chosen environment; a function call only the caller's connections; another space's or user's connections are unreachable.
- **Runs are separated:** a runner can't reach another run's runners or fetcher, nbsandboxd, the host, the cloud metadata address or the internet; a token from one run is useless in another and after the run.
- **Egress is bounded:** the gateway refuses endpoints that aren't the run's connections, and addresses outside the operator's allowed ranges. The fetcher refuses HTTP requests outside a connection's URL, for names the run doesn't have, and without the run's token.
- **The fetcher stays locked:** no file or URL access through SQL, no extension loading, its own limits.
- **Audit:** connection and environment changes, and every read (run, connection, table).

## Order of work

Each phase usable on its own:
1. **Connections, secret providers, names and environments;** mediated reads with one runner plus a fetcher (PostgreSQL, MySQL, SQL Server, object storage with Parquet); the egress gateway; runs, sessions and function calls with the caller's connections; the isolation rows. **Built** (M0 to M5, 2026-11-25).
2. **HTTP APIs through the fetcher** ([design](#http-apis-through-the-fetcher)), in four milestones:
   - H1: the fetcher's HTTP port with `header` and `none` auth, `nbsandbox.http()` and `connection()`, on Docker, with tests against a local API (JSON, SSE, uploads, refused paths and tokens);
   - H2: `basic` and `oauth2` (against Keycloak, already in the weekly run), and `ca`;
   - H3: Kubernetes (the NetworkPolicy's port), the isolation-report rows, the soak's reading space calling an API (`nbsandbox-report check` has no connections section yet, for databases either);
   - H4: the web UI's connection form, the API's description of the settings, docs, the tour (gRPC takes settings as a Struct and MCP has no connection tools: nothing to change; no example notebook, as the dev stack has no API to call).
3. **Iceberg tables and catalogs.**
4. **Runs with several runners:** orchestrator and workers, splits, partial aggregation with DuckDB, limits per run (and so run sizes).
5. **A distributed engine,** when a workload needs one.
6. **Writes to external sources.**
7. **Injected mode,** when a client needs the raw credential.

Shared spaces and their per-member connections follow the shared-spaces item.

## Spike results (M0, 2026-11-25)

Measured on Docker Desktop (Apple Silicon, 4 CPUs, 3 GB) and the local kind cluster (Kubernetes 1.37, Cilium), with 10 million rows of Parquet (88 MB; 467 MB as Arrow) and a PostgreSQL 17 source.

**The fetcher's language.** The same fetcher (DuckDB, an Arrow Flight server, one CPU, 1 GiB):

| | Stream 10 M rows | Aggregate to 4 rows | Image |
|---|---|---|---|
| Python (`duckdb`, `pyarrow.flight`) | **0.6–1.1 s** | 0.16 s | 592 MB (with the PostgreSQL extension; not slimmed) |
| Go (`duckdb-go`, `arrow-go`) | 14–25 s | 0.4–0.9 s | 256 MB |

Go's cost is `duckdb-go`'s Arrow export itself: 22 s for the 10 M rows with no network at all (4.5 s for one integer column), in 2,048-row records; merging them into larger batches didn't help. Python's `duckdb` uses DuckDB's own C++ Arrow export, and `pyarrow`'s Flight is C++ too.

**DuckDB versions.** In one process, streaming the 10 M rows: 1.5.6 0.64 s; 2.0.1 1.5–3.6 s; under the Flight server, 2.0.1 took 38 s (2.0 also changed the Python API: `fetch_record_batch` and `fetchone` on relations are gone). 1.5.6 behaves like 1.4.2 in every test below.

**The lockdown, attacked with 26 queries** (the same results on 1.4.2, 1.5.6 and 2.0.1):
- **Turning external access off also blocks the fetcher's own sources,** since views are read when queried: the sources must be allowed explicitly (`allowed_directories`, set before external access is turned off).
- **Blocked:** reading files (`/etc/passwd`, the mounted secret, `/proc/self/environ`, path traversal, globbing), environment variables (DuckDB has no `getenv`), unlocking the configuration, loading or installing extensions, HTTP, writing files, attaching other databases, tables the database role can't read, writes through a read-only attachment, `pg_read_file` (the role lacks it).
- **A leak, fixed:** `duckdb_databases()` shows an attachment's connection string, password included. With DuckDB's secret manager (`CREATE SECRET … TYPE postgres`, attached by name) the path is empty and the secret shows only redacted (`password=redacted`); unredacted display is refused.
- **Open by default:** raw SQL to the source (`postgres_query`, `postgres_execute`), and statements other than queries (`CREATE TABLE`, `PRAGMA`).
- **A gate closes both:** DuckDB's own parser (`json_serialize_sql`) serializes only a single `SELECT`, and its output names every function called, so the fetcher refuses anything else, and pass-through functions, before running it.
- So three layers: the gate (one query, no pass-through); DuckDB's settings (only the run's sources, no network, no extensions, configuration locked); the database role (read-only, least privilege).

**SQL Server through ODBC.** Microsoft's ODBC Driver 18 installs and loads on arm64 (Debian 12); its licence is in the package. DuckDB's ODBC extension (1.5/2.0) has no `ATTACH`, only query text (`odbc_query`), so no pushdown: a view over a table transfers the whole table. With PostgreSQL's ODBC driver it crashed (a segmentation fault) on integer and numeric columns. No real SQL Server could run here (its image is amd64-only and needs over 2 GB beside the kind cluster).

**Egress on kind, with plain NetworkPolicy only** (a deny-all policy, one policy per run, fetchers to the gateway; a 15 MB Go gateway with HMAC-signed run tokens):
- the fetcher reaches its source only through the gateway, and only the source in its token (another one: 403; an expired or forged token: 407; each decision logged);
- the fetcher can't connect to the source directly, and can't resolve names (no DNS in runs);
- a runner reaches its own fetcher, but not another run's runner or fetcher, nor the gateway (mediated mode), the Kubernetes API or nbsandbox's own Pods.

Verified with Cilium and Calico (kind, 2026-11-26; the weekly run repeats both); the policies use only standard NetworkPolicy, still to be checked with Azure's policy engine.

## What others do (research, 2026-11-24)

- **Spark:** splits data into partitions read in parallel by executors directly from storage, spills to disk, recomputes lost work. Executors hold the source credentials (cloud roles, keytabs, JDBC passwords). Spark Connect separates a thin client from the engine (plans over gRPC, Arrow results).
- **Snowflake (Snowpark):** user Python runs in a gVisor sandbox with no network by default; administrators open named destinations with network rules and secrets. Not self-hostable, but the closest model to nbsandbox's.
- **Databricks (Lakeguard):** user code isolated in containers from the engine and from other users, egress controlled, data access enforced by the catalog. Not self-hostable.
- **BigQuery DataFrames, Ibis:** pandas-like code compiled to SQL and run where the data is.
- **MotherDuck:** one query split between a local DuckDB and the cloud; only intermediate results move.
- **Polars Cloud, Daft, Ray:** distributed DataFrames in Python or Rust; Ray on Kubernetes is usually given a namespace and network policies per team.
- **Iceberg and Parquet with DuckDB:** reading in place, skipping files, row groups and columns; Iceberg snapshots version data without copies.
- **Modal:** sandboxes mount buckets that the host fetches lazily.
- **Secrets:** Airflow and Grafana keep self-service secrets encrypted in their own databases and also read from external backends (Vault, cloud secret managers); Databricks has scopes it stores and scopes backed by Azure Key Vault; Kubernetes-native tools reference Kubernetes Secrets.

**What we took from it:** read in place and move only what's needed (Parquet and Iceberg, DuckDB); keep credentials out of user code (Snowpark, Lakeguard), with the database's own permissions as the boundary; split work per run, as Spark's driver and executors do, but with nbsandbox's isolation around every part; deny network by default and open only named endpoints (Snowpark).

**What we didn't:** requiring a vendor platform or an existing cluster; copying data into nbsandbox to make it readable; giving notebooks direct network access in mediated mode.

## Decisions so far

- Kubernetes only (AKS and on-premises); Docker for development.
- Secret providers: Kubernetes Secrets (with Sealed Secrets and External Secrets Operator) and Vault/OpenBao; a built-in store for development only. Each secret managed or referenced. Kubernetes Secrets live in the runs namespace, where the fetcher Pods that mount them run.
- Connections at user level; names per space; environments; no "protected" environment.
- Functions and shared spaces use the caller's or member's own connections.
- Mediated mode for SQL and files; HTTP APIs through the fetcher (2026-11-27); injected mode only when a client needs the raw credential.
- A run = an orchestrator, workers and a fetcher, on a private network per run.
- The broker (resolving names and permissions, auditing) inside nbsandboxd; the fetcher a per-run Pod of its own image.
- DuckDB first; other engines only for the cases above.
- The fetcher in Python, with DuckDB pinned to the 1.5 series (2.0 slower and its Python API changed); plain Arrow Flight, not Flight SQL.
- Databases: DuckDB's core extensions (PostgreSQL, MySQL); SQL Server through the community `mssql` extension, validated in CI; ODBC only as a last resort.
- Runs with workers start elastically, once a minimum fraction of their requested workers can be placed; nbsandbox's own admission, no external scheduler.
- Simple reads by default; pass-through per connection.
- Egress through our own Go gateway (one per cluster, with replicas; a command of the nbsandboxd binary), with each run's allowed endpoints in a signed token, so plain NetworkPolicy is enough everywhere.
- Object storage: any credential that is a secret (access keys, connection strings, SAS tokens). Workload identity (AKS) is parked: it changes how the fetcher Pod is set up, not the design.
- Results streamed or written as artifacts; external writes later.
- On Docker, a private `internal` network per run (runner and fetcher; it works under gVisor, unlike a shared Unix socket), and the gateway as on Kubernetes: one egress path for both backends. The socket proxy allows creating, connecting and removing networks.
- The run ↔ fetcher contract is plain Arrow Flight with two environment variables, so any image can use it; `pyarrow` joins the runner image.
- Runs choose an environment (`"environment"`, the space's default otherwise) and record which connection each name meant; the fetcher starts only for runs whose space binds names.
- M2 covers runs; sessions and functions get their fetchers in M4.
- The gate also refuses `query()` and `query_table()` (they run SQL from a string), `duckdb_*` and `pragma_*` functions, and `..` in a table function's paths (DuckDB's `allowed_directories` doesn't normalise `s3://bucket/../other`). Found by fuzzing the gate (2026-11-25), it also refuses paths and URLs in `FROM` (replacement scans, which name no function) and table function arguments that aren't written out as values (a computed `chr(46) || chr(46)` would hide a `..`); the fuzzer (`fetcher-image/tests/test_gate_fuzz.py`) and the spike's attack set run with `make fetcher-test`. Secrets are kept in memory only (`allow_persistent_secrets` off), created before the lockdown.
- The fetcher attaches every source when it starts; one it can't attach (a wrong password, a source down) fails the run at start with the fetcher's own message. Its output until it serves goes to the run's log, prefixed `[fetcher]`.
- On Docker, the fetcher's configuration, credentials included, is in its container's environment, dropped from the process once read: no file on the host, visible only to whoever holds the Docker API, who holds the host anyway. On Kubernetes it is a per-run Secret. The fetcher has limits of its own, not the run's: reserving the run's memory twice kept run Pods from being scheduled (found on kind). DuckDB keeps under three quarters of them and spills to `/tmp`.
- On Kubernetes (M3): the gateway is a Deployment and Service in the release namespace, reachable only by fetchers. A reading run gets a fetcher Pod (no DNS: the worker resolves the gateway's Service to its address), a per-run Secret with its configuration and tokens, and two per-run NetworkPolicies (its runner may reach its fetcher, nothing else may), on top of the namespace's deny-all; one chart policy lets fetchers reach the gateway's port. Connections' Kubernetes Secrets are mounted into the fetcher, never read by nbsandboxd; the API processes get a token only to manage Secrets in the runs namespace, and only with the `kubernetes` provider. The run Pod is created once the fetcher serves, with its Pod address.
- M4: MySQL through DuckDB's `mysql` extension, SQL Server through the community `mssql` extension (checked against DuckDB 1.5.6: `ATTACH` with a DuckDB secret, read-only, filters and columns pushed down; SQL Server 2022 in the tests, amd64). The fetcher allows community extensions only when a run has a SQL Server source, to load the copy in its image; once locked, nothing else can be installed or loaded.
- Sessions resolve their names when opened and keep them; their fetcher lives as long as the session, under labels of its own so the run reconciler never removes it.
- A published version declares the names it reads and their types (its space's bindings when published). A call needs an environment binding all of them, to connections of the same types, or it is refused before anything runs; the run gets only the declared names. Functions are invoked by their space's owner today, so "the caller's connections" are the space's bindings; shared spaces move it to each caller's own.
- M5, Vault/OpenBao (decided 2026-11-25): the fetcher logs in with Vault's Kubernetes auth, not a per-run policy (which would need nbsandboxd to write Vault policies, close to administering Vault) and not values passed by nbsandboxd. Scope is per installation: the fetcher role's policy covers nbsandbox's own KV path and the reference prefixes the operator allows; each fetcher reads only its run's paths and revokes its token. The fetcher reaches Vault through the gateway (its address joins the run's token), with a projected service account token for the `vault` audience, useless against the Kubernetes API. nbsandboxd logs in the same way (or with a token) to write managed secrets under its prefix and check that references exist, never reading a value. Kubernetes only, as the `kubernetes` provider; the built-in store remains for Docker. A small client of nbsandbox's own (KV v2 and Kubernetes login are a few HTTP calls), not Vault's Go library.
- The gateway token lives until the run's time limit plus five minutes; a connection deleted, or pointing somewhere the operator no longer allows, fails a queued run at start.
- Live data is the source's concern: for repeatable results, read a snapshot.
- TLS to sources (2026-11-25): databases take `tls` (`off`, `prefer`, `require`, `verify`) and `ca`; the default stays `prefer`, the drivers' own (encrypted when offered, unchecked), with `verify` a choice. The fetcher reaches databases through a forwarder on loopback, so a certificate can't be checked against the name connected to: Postgres connects to `hostaddr=127.0.0.1` with `host` set to the source's name; MySQL's client skips the name check for 127.0.0.1 and has no `hostaddr`, so a source with `verify` gets a host alias (Docker `--add-host`, Kubernetes `hostAliases`) from its name to a loopback address of its own, where its forwarder listens. SQL Server can't verify: the community `mssql` extension published for DuckDB 1.5.6 (0.2.5) encrypts but accepts any certificate (`TrustServerCertificate` and `HostNameInCertificate` come in its 0.3.0), so `verify` is refused for it. S3 over HTTPS was already checked (curl, the system's authorities); `ca` adds a private authority (DuckDB's `ca_cert_file`, one bundle for all of a run's stores).
- HTTP APIs (2026-11-27): the notebook's code calls through its run's fetcher, which adds the credential; the run's token stands in for it. Auth `header`, `basic`, `oauth2` (client credentials) and `none`; a connection allows its URL's host and paths under its path, any method, the credential's own permissions being the limit. Injected mode is deferred until a client needs the raw credential.
