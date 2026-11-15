# MCP: nbsandbox for AI agents

`nbsandbox-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server. With it, an AI agent (Claude Code, Claude Desktop, an IDE assistant, your own agent) can write a notebook into a space, run it in the sandbox and read the result: an agent gets somewhere safe to run the Python it writes, with no network and fixed limits, and every run is kept with its log, outputs and artifacts like any other.

It's a thin layer over the [REST API](api.md): every tool is one or two API calls, made **as the agent's user**. It holds no data and grants nothing; nbsandbox checks every call as usual.

## Tools

| Tool | What it does |
|---|---|
| `nbsandbox_runtime` | Python version, installed packages and per-run limits (runs have no network: a package not listed can't be installed in a run; with custom packages on, the space's `requirements.txt` adds packages: [packages.md](packages.md)) |
| `list_spaces`, `create_space` | the user's spaces |
| `list_files`, `read_file`, `write_file` | the space's files; `write_file` writes text (a notebook as nbformat 4 JSON, a CSV), creating folders |
| `run_notebook` | run a notebook with parameters (optionally `read_only`) and wait for its result, up to `wait_seconds` (default 120, at most 600) |
| `get_run_result`, `get_run`, `list_runs` | keep waiting for a result; a run's details; the space's runs |
| `get_run_log` | the end of a run's log (up to 64 KiB) |
| `cancel_run` | stop a run; its partial output is kept |
| `list_artifacts`, `read_artifact` | the files a run wrote to its artifacts folder; text ones are returned (up to 64 KiB) |
| `list_functions`, `get_function`, `invoke_function` | published functions: their contracts, and calls with parameters |

Tools are marked read-only, or as making changes (`write_file` and `cancel_run` as destructive), so clients can ask before the ones that change things. Long texts are cut with a note saying how much was left out.

## Local: the agent starts it (stdio)

Get `nbsandbox-mcp` from a release archive, or build it: `go build ./cmd/nbsandbox-mcp`. It acts as one user, from the environment:

- `NBSANDBOX_URL`: nbsandbox's address (`https://…`; plain `http` only to localhost or a private address);
- `NBSANDBOX_TOKEN`: an OIDC access token, or `NBSANDBOX_USER` and `NBSANDBOX_PASSWORD` (basic auth, for the users file).

**Claude Code:**

```bash
claude mcp add nbsandbox -e NBSANDBOX_URL=https://nbsandbox.example.com -e NBSANDBOX_TOKEN="$TOKEN" -- nbsandbox-mcp
```

**Claude Desktop and other clients** (their MCP configuration file):

```json
{
  "mcpServers": {
    "nbsandbox": {
      "command": "/usr/local/bin/nbsandbox-mcp",
      "env": { "NBSANDBOX_URL": "https://nbsandbox.example.com", "NBSANDBOX_TOKEN": "…" }
    }
  }
}
```

Against the local dev stack (`make up`): `NBSANDBOX_URL=http://127.0.0.1:8080 NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password`.

## Shared: one server for a team (HTTP)

```bash
NBSANDBOX_URL=http://nbsandbox-api:8080 nbsandbox-mcp -http 0.0.0.0:8095
```

It serves MCP's streamable HTTP transport on `/mcp` (stateless; `/healthz` for probes). It has **no credentials of its own**: each request must carry its caller's `Authorization` header (a bearer token, or basic auth), which is passed to nbsandbox unchanged, so every agent acts as its own user. Requests without one get 401. Put it behind your TLS ingress, like the API.

## Scopes

With OIDC, the agent's token needs the scopes of the tools it uses:

| Scope | For |
|---|---|
| `nbsandbox.read` | every listing and read, results and logs |
| `nbsandbox.spaces.write` | `create_space` |
| `nbsandbox.files.write` | `write_file` |
| `nbsandbox.runs` | `run_notebook`, `cancel_run` |
| `nbsandbox.functions.invoke` | `invoke_function` |

A token with only `nbsandbox.read` and `nbsandbox.functions.invoke` gives an agent the published functions and nothing else: a safe way to let an agent use notebooks someone else wrote and reviewed.

## Notes

- **The sandbox applies to what the agent runs.** Notebooks run with the same isolation as any run: no network, the run's limits, its own copy of the space. Files a run writes are written back to the space when it ends (unless `read_only`), so an agent can change a space's files through runs as well as through `write_file`; give it its own space.
- **gRPC:** the MCP server uses the REST API. The gRPC services offer the same operations for programs that prefer them ([api.md](api.md)).
