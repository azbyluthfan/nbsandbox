# Plan: data outside nbsandbox

**Status (2026-11-24):** design under discussion; not scheduled. Getting the design right comes first; when and in what order to build it is decided later. The [ROADMAP](../ROADMAP.md) links here.

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
| HTTP | the customer's own APIs, including streaming (SSE) | through the notebook's own code ([injected mode](#two-modes-mediated-and-injected)) |

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
- **managed:** the user enters it in nbsandbox's UI or API (write-only: it is never shown again), and nbsandbox writes it into **a namespace or Vault path that only nbsandbox owns**. nbsandbox's permissions are limited to that namespace or path. This is the self-service way, as Airflow's or Grafana's own stores are; or
- **referenced:** the connection points to an existing Kubernetes Secret or Vault path, created outside nbsandbox (GitOps, Sealed Secrets, the platform team). Read-only, as Airflow's secrets backends or Databricks' Key Vault-backed scopes work.

**nbsandboxd never handles a secret's value when a run starts.** It creates the run's fetcher Pod with *references* to exactly the secrets that run needs, and the kubelet mounts them. With Vault, the fetcher gets a short-lived token limited to those paths.

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
- `nbsandbox.connections()` lists the names a run can use and their tables, never hosts or secrets. The editor can complete `crm.` with table names.

**How a name is resolved, all on nbsandbox's side:**

```
run → its space → the chosen environment → the name → a connection → its secret (reference)
```

Every run records its environment and which connection each name meant.

### Functions and shared spaces: the caller's connections

- **A function call resolves names with the caller's own connections.** The publisher declares the names a function needs and what they must be (`crm: postgresql, tables orders, customers`). A caller without a matching connection gets a clear error before anything runs. Each run records whose connections it used. Nothing is pinned to the publisher's credentials, so nothing breaks when the publisher leaves.
- **Shared spaces** (on the ROADMAP; no organisations): each member runs the space's notebooks with **their own connections** under the space's names.

### Two modes: mediated and injected

| | Mediated (the default) | Injected |
|---|---|---|
| For | SQL and files: the database and object-storage types | anything the notebook's own code must call: HTTP and SSE APIs, a storage layer without a built-in type, a vendor SDK |
| Who connects | the run's **fetcher** | the **notebook's code** |
| The secret | stays in the fetcher; never in the run | given to the run (environment or file), **redacted** from logs, outputs and results |
| The run's network | only to its own fetcher | also to that connection's endpoint, through the egress gateway |
| What the notebook gets | Arrow data: `nbsandbox.sql(...)`, `nbsandbox.stream(...)` | `nbsandbox.connection("events_api")`: endpoint, headers or token, for `requests`, an SSE client or an SDK |

Injected mode is the user's own secret in the user's own code. The risk is accidental exposure, which redaction and the single allowed endpoint limit. Mediated mode is preferred wherever it fits. Injected mode replaces the ROADMAP's earlier "network access to registered endpoints" item.

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

- **Its own image, one Pod per run**, with the run's limits. It never runs notebook code. It holds only that run's resolved credentials.
- **DuckDB** as the engine, with **external access locked** (no reading local files or arbitrary URLs from SQL, configuration locked, no community extensions), since its SQL comes from the notebook. DuckDB's own docs say SQL from untrusted sources needs sandboxing; the fetcher Pod is that sandbox.
- **Reads in place:** Parquet and Iceberg statistics skip files and row groups, and only the needed columns are read.
- **Databases:** PostgreSQL and MySQL through DuckDB's own core extensions; databases DuckDB has no connector for, SQL Server first, through ODBC (DuckDB's ODBC extension with the database vendor's driver, Microsoft's for SQL Server). The same connectors can write later.
- **Simple reads by default:** a source receives only generated scans (these columns, these filters), and joins and aggregation run in DuckDB, so nbsandbox controls exactly what each source sees. Passing more SQL through to a source can be enabled per connection, relying on its database role.
- **Serves Arrow over Flight SQL** to the run's runners. A query's result can be split, one stream per worker, which is the basis of distributed mode.
- **The source's own permissions are the real boundary.** Each connection's database user should be **read-only and least-privilege in the database**. nbsandbox's table lists help users; they are not the security guarantee, and the docs and the connection form say so.

### Egress on AKS and on-premises

Kubernetes NetworkPolicy works on IP addresses. DNS-name rules exist only in some engines (Cilium; on AKS, an add-on), so they aren't portable. Instead:
- **Runs and fetchers may reach only the egress gateway** (plus their run's private network): plain NetworkPolicy, enforced by every engine AKS offers and by Calico or Cilium on-premises. nbsandbox already refuses to run where network policies aren't enforced.
- **The egress gateway** is our own small proxy in Go: a role of the nbsandboxd binary (`role: gateway`), **one per cluster**, run with several replicas. It **resolves names itself** and allows only the run's connection endpoints: HTTPS by name (`CONNECT`), databases as a TCP tunnel per connection. Each connection is logged.
- **A run's allowed endpoints reach the gateway in a signed token.** When a run starts, nbsandboxd issues it a token naming the run, its allowed `host:port` list and an expiry. The gateway only checks the signature: it keeps no state and makes no lookup per connection, and the token dies with the run.
- **Injected mode:** notebook database clients can't talk to a proxy, so the runner starts a small forwarder inside the run, and `nbsandbox.connection("crm")` gives the notebook a local address (`127.0.0.1:15432`) that tunnels through the gateway. HTTP clients use `HTTPS_PROXY`.
- It can later move to one gateway per node, or to Envoy, if traffic calls for it, without changing the rest of the design.
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
- **Secrets stay out of runs** in mediated mode: absent from the run's environment, files, logs, outputs and API responses. Injected secrets are redacted wherever nbsandbox writes text.
- **Names are bounded:** a run can use only its space's names in the chosen environment; a function call only the caller's connections; another space's or user's connections are unreachable.
- **Runs are separated:** a runner can't reach another run's runners or fetcher, nbsandboxd, the host, the cloud metadata address or the internet; a token from one run is useless in another and after the run.
- **Egress is bounded:** the gateway refuses endpoints that aren't the run's connections, and addresses outside the operator's allowed ranges.
- **The fetcher stays locked:** no file or URL access through SQL, no extension loading, the run's limits.
- **Audit:** connection and environment changes, and every read (run, connection, table).

## Order of work (to decide later)

A possible order, each phase usable on its own:
1. **Connections, secret providers, names and environments;** mediated reads with one runner plus a fetcher (PostgreSQL, MySQL, SQL Server, object storage with Parquet); the egress gateway; runs, sessions and function calls with the caller's connections; the isolation rows.
2. **Injected mode** (HTTP and SSE APIs, custom storage) with redaction.
3. **Iceberg tables and catalogs.**
4. **Runs with several runners:** orchestrator and workers, splits, partial aggregation with DuckDB, limits per run (and so run sizes).
5. **A distributed engine,** when a workload needs one.
6. **Writes to external sources.**

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

Verified with Cilium; the policies use only standard NetworkPolicy, still to be checked with Calico and Azure's policy engine.

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
- Secret providers: Kubernetes Secrets (with Sealed Secrets and External Secrets Operator) and Vault/OpenBao; a built-in store for development only. Each secret managed or referenced.
- Connections at user level; names per space; environments; no "protected" environment.
- Functions and shared spaces use the caller's or member's own connections.
- Mediated mode for SQL and files, injected mode for APIs and custom storage.
- A run = an orchestrator, workers and a fetcher, on a private network per run.
- The broker (resolving names and permissions, auditing) inside nbsandboxd; the fetcher a per-run Pod of its own image.
- DuckDB first; other engines only for the cases above.
- Databases: DuckDB's core extensions (PostgreSQL, MySQL); ODBC for those DuckDB has no connector for (SQL Server first).
- Runs with workers start elastically, once a minimum fraction of their requested workers can be placed; nbsandbox's own admission, no external scheduler.
- Simple reads by default; pass-through per connection.
- Egress through our own Go gateway (one per cluster, with replicas; a role of the nbsandboxd binary), with each run's allowed endpoints in a signed token, so plain NetworkPolicy is enough everywhere.
- Object storage: any credential that is a secret (access keys, connection strings, SAS tokens). Workload identity (AKS) is parked: it changes how the fetcher Pod is set up, not the design.
- Results streamed or written as artifacts; external writes later.
- Live data is the source's concern: for repeatable results, read a snapshot.
