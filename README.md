# mcp-proxy

A stateless, multi-tenant **Model Context Protocol** server over SSE that lets AI
clients (Cursor, Claude Desktop) connect to **any** PostgreSQL database by passing
credentials as HTTP headers.

There is no user table, no credential vault and no session store. Each client
supplies its own target; a connection pool is created for the life of that
client's SSE stream and destroyed the moment the stream closes.

Ships as **one static binary** with no runtime dependencies, run under systemd.

---

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Connecting a client](#connecting-a-client)
- [The serialization matrix](#the-serialization-matrix)
- [Safety model](#safety-model)
- [Configuration](#configuration)
- [Deployment](#deployment)
- [Connecting a domain](#connecting-a-domain)
- [Operations](#operations)
- [Testing](#testing)

---

## How it works

```
  AI client                    droplet                        tenant database
 ┌──────────┐   GET /v1/mcp/connect   ┌───────────┐
 │  Cursor  │ ─── x-db-* headers ───► │   nginx   │
 │  Claude  │ ◄── SSE event stream ── │  (TLS)    │
 └──────────┘                         └─────┬─────┘
                                            │ 127.0.0.1:8080
                                      ┌─────▼──────────────┐
                                      │    mcp-proxy       │
                                      │                    │
                                      │ 1 stream           │
                                      │   = 1 MCP server   │     ┌─────────────┐
                                      │   = 1 pgx pool  ───┼────►│ PostgreSQL  │
                                      │                    │     └─────────────┘
                                      │ stream closes      │
                                      │   → pool destroyed │
                                      └────────────────────┘
```

**Request lifecycle**

1. `GET /v1/mcp/connect` — middleware validates the `x-db-*` headers. Anything
   wrong returns `400` listing every problem at once.
2. The SDK opens an SSE stream and calls our `getServer` hook, which builds a
   `Session` (credentials + lazy pool) and an MCP server bound to it.
3. The client POSTs JSON-RPC messages back to the same path with the
   server-issued `?sessionid=`.
4. The first `execute_query` creates the pool. Streams that never query never
   open a database connection.
5. When the client disconnects, `r.Context()` fires. The session cancels
   in-flight queries **first**, then closes the pool — so `pgxpool.Close` never
   blocks on a busy connection.

### Layout

| Path | Responsibility |
|---|---|
| `cmd/mcp-proxy` | Entrypoint, HTTP server, two-phase graceful shutdown |
| `internal/config` | Environment config with safe fallbacks |
| `internal/logging` | Structured JSON logging to **stderr only** |
| `internal/dbconn` | Header → credentials → DSN; per-stream pool lifecycle |
| `internal/guard` | SQL screening (comment/literal-aware) |
| `internal/serialize` | The three-tier response matrix |
| `internal/mcpsrv` | MCP tool surface, query execution |
| `internal/httpapi` | Router, middleware, SSE wiring |
| `deploy/` | systemd unit, env template, nginx config |

---

## Quick start

Requires Go 1.25+ only to build; the produced binary needs nothing.

```bash
make verify                      # gofmt, go vet, tests with -race
make build                       # binary for this machine

MCP_LISTEN_ADDR=127.0.0.1:8080 ./bin/mcp-proxy-binary
```

Confirm it is alive and that the header contract is enforced:

```bash
curl -s localhost:8080/healthz

# Missing headers → an actionable 400
curl -s localhost:8080/v1/mcp/connect

# A real SSE handshake
curl -sN -H 'x-db-username: postgres' -H 'x-db-password: secret' \
        -H 'x-db-host: 127.0.0.1'    -H 'x-db-port: 5432' \
        localhost:8080/v1/mcp/connect
# → event: endpoint
#   data: /v1/mcp/connect?sessionid=...
```

---

## Connecting a client

### Cursor — `~/.cursor/mcp.json`

```json
{
  "mcpServers": {
    "postgres-proxy": {
      "url": "https://dbmcp.hookman.online/v1/mcp/connect",
      "headers": {
        "x-db-username": "app_reader",
        "x-db-password": "REPLACE_ME",
        "x-db-host": "db.example.com",
        "x-db-port": "5432",
        "x-db-database": "appdb",
        "x-db-sslmode": "require"
      }
    }
  }
}
```

### Claude Desktop — via `mcp-remote`

Claude Desktop launches local processes, so a remote server is reached through
the `mcp-remote` bridge:

```json
{
  "mcpServers": {
    "postgres-proxy": {
      "command": "npx",
      "args": [
        "-y", "mcp-remote",
        "https://dbmcp.hookman.online/v1/mcp/connect",
        "--header", "x-db-username:app_reader",
        "--header", "x-db-password:REPLACE_ME",
        "--header", "x-db-host:db.example.com",
        "--header", "x-db-port:5432",
        "--header", "x-db-database:appdb",
        "--header", "x-db-sslmode:require"
      ]
    }
  }
}
```

### Headers

| Header | Required | Default |
|---|---|---|
| `x-db-username` | yes | — |
| `x-db-password` | yes (may be empty for `trust`/`peer` auth) | — |
| `x-db-host` | yes | — |
| `x-db-port` | yes | — |
| `x-db-database` | no | `postgres` |
| `x-db-sslmode` | no | `prefer` |

Credentials are used in request scope and never logged, cached or persisted.
Logs carry a non-reversible `fingerprint` derived from user/host/port/database —
never the password — so you can correlate a tenant's lines without exposing them.

### Tools

| Tool | Purpose |
|---|---|
| `execute_query` | Run one read-only statement |
| `list_tables` | Tables and views, optionally filtered by schema |
| `describe_table` | Columns, types, nullability, defaults |
| `ping_database` | Verify credentials and report server version |

---

## The serialization matrix

A JSON array of objects repeats every key on every row. For a typical
eight-column result the keys, quotes, colons and braces are over half the
payload. CSV pays for the column names once, so the same rows cost roughly a
third of the tokens. The server switches representation on size:

| Tier | Rows | Format |
|---|---|---|
| 1 | `0 … MCP_JSON_MAX_ROWS` (100) | structured JSON, column order preserved |
| 2 | up to `MCP_CSV_MAX_ROWS` (2000) | flat CSV |
| 3 | beyond that | CSV truncated at the ceiling + truncation warning |

Every response opens with a metadata block whose **first line declares the
format**, so the model never infers the shape of what it received:

```
Format: text/csv
Rows: 2000 (TRUNCATED at the 2000 row ceiling; true total unknown)
Columns: 2 — id:int4, label:text
Nulls: SQL NULL is written as \N; an empty field is a genuine empty string
Encoding: RFC 4180 CSV, comma delimited, first line is the header row
Elapsed: 182ms

id,label
1,row-1
...

⚠️ TRUNCATION WARNING: Dataset exceeded max allowable limit of 2000 rows.
...
```

Design decisions worth knowing:

- **Truncation is stated twice** — in the metadata and in a trailing banner.
  The failure that actually matters is a model receiving a silent sample and
  reporting its row count as the true total.
- **A fourth, independent ceiling: `MCP_MAX_RESULT_BYTES`.** Row counts do not
  bound memory. 2000 rows each holding a 1 MB `bytea` is 2 GB on a 1 GB droplet.
  Tripping the byte budget produces its own truncation warning.
- **NULL is `\N`, not an empty field.** An empty field cannot be distinguished
  from a genuine empty string, and that ambiguity silently corrupts downstream
  aggregation. The encoding is declared in the metadata.
- **Results are read in PostgreSQL text format and passed through verbatim.**
  `uuid`, `numeric`, arrays, ranges, enums, geometry and extension types all
  render as the server renders them — no client-side type table to drift, and no
  `numeric` rounded through a `float64`.
- **One row beyond the ceiling is fetched, never counted.** That extra row is
  what proves more data exists, so memory stays bounded regardless of table size.

---

## Safety model

**Stated plainly: the keyword scanner is a guardrail, not a security boundary.**
It stops a confused model from issuing a destructive statement by accident. It
cannot stop a determined adversary, because SQL has too many ways to express the
same intent.

**The real boundary is the privileges of the role in `x-db-username`.** Give it
`SELECT` and nothing else:

```sql
CREATE ROLE app_reader LOGIN PASSWORD 'REPLACE_ME';
GRANT CONNECT ON DATABASE appdb TO app_reader;
GRANT USAGE ON SCHEMA public TO app_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO app_reader;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO app_reader;
```

Then set `MCP_READ_ONLY_TX=true`, which wraps every statement in a `READ ONLY`
transaction — enforced by PostgreSQL itself, not by pattern matching.

What the scanner does buy over a naive `strings.Contains`:

- Comments are replaced by whitespace *before* matching — the same thing
  PostgreSQL's lexer does — so `DROP/**/DATABASE x` is caught.
- String literals, quoted identifiers and dollar-quoted bodies are stripped, so
  `SELECT 'drop database x'` and a column named `vacuum_log` do **not** false
  positive.
- Stacked statements (`SELECT 1; DROP DATABASE x`) are rejected outright.

Blocked by default: `DROP DATABASE`/`SCHEMA`/`TABLESPACE`/`ROLE`/`OWNED`,
`VACUUM`, `TRUNCATE`, `ALTER SYSTEM`, `REINDEX`, `CLUSTER`, `COPY … PROGRAM`
(shell execution on the DB host), `pg_read_file`/`pg_ls_dir`, `lo_import`/
`lo_export`, `pg_terminate_backend`, `LANGUAGE C` functions, and anonymous
`DO` blocks. Extend with `MCP_EXTRA_BLOCKED_PATTERNS`.

Other protections:

- **Credentials cannot restructure the DSN.** It is assembled with `net/url`, so
  a password containing `@ : / ? #` is escaped rather than reinterpreted.
- **Link-local addresses are always refused**, because `169.254.169.254` is the
  cloud instance-metadata endpoint and no legitimate database lives there.
  `MCP_BLOCK_PRIVATE_HOSTS=true` additionally refuses loopback and RFC1918,
  re-checked after DNS resolution.
- **`Credentials` implements `Stringer` and `slog.LogValuer`**, so even an
  accidental `%v` or logger call emits a redacted form. Driver errors are
  scrubbed of the password before they are returned or logged.
- **Panics are contained.** Recover middleware turns a handler panic into a 500;
  one tenant cannot take down other tenants' streams.

---

## Configuration

Every value has a safe fallback. An unparseable or out-of-range value logs a
warning and uses the default — a typo in a unit file never stops the service
from booting.

| Variable | Default | Purpose |
|---|---|---|
| `MCP_LISTEN_ADDR` | `:8080` | Bind address |
| `MCP_JSON_MAX_ROWS` | `100` | Tier 1 ceiling |
| `MCP_CSV_MAX_ROWS` | `2000` | Tier 2 ceiling / truncation point |
| `MCP_MAX_RESULT_BYTES` | `33554432` | Independent byte ceiling (32 MB) |
| `MCP_DEFAULT_DATABASE` | `postgres` | Default for `x-db-database` |
| `MCP_DEFAULT_SSLMODE` | `prefer` | Default for `x-db-sslmode` |
| `MCP_QUERY_TIMEOUT_MS` | `30000` | Per-statement deadline |
| `MCP_CONNECT_TIMEOUT_MS` | `10000` | Dial timeout |
| `MCP_POOL_MAX_CONNS` | `4` | Connections **per stream** |
| `MCP_POOL_MAX_CONN_IDLE_MS` | `60000` | Idle connection reaping |
| `MCP_POOL_MAX_CONN_LIFETIME_MS` | `1800000` | Connection recycling |
| `MCP_READ_ONLY_TX` | `false` | Wrap statements in `READ ONLY` transactions |
| `MCP_BLOCK_PRIVATE_HOSTS` | `false` | Refuse loopback/RFC1918 targets |
| `MCP_EXTRA_BLOCKED_PATTERNS` | — | Comma-separated extra regexes |
| `MCP_DISABLE_LOCALHOST_PROTECTION` | `false` | **Required behind a reverse proxy** — see below |
| `MCP_SHUTDOWN_GRACE_MS` | `15000` | Graceful shutdown budget |
| `MCP_IDLE_TIMEOUT_MS` | `120000` | HTTP keep-alive idle timeout |
| `MCP_LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |

> **`MCP_DISABLE_LOCALHOST_PROTECTION` is the one setting that will waste your
> afternoon.** The SDK rejects requests that arrive from a loopback address but
> carry a non-loopback `Host` header — a DNS-rebinding defence. Behind nginx on
> the same droplet, *every* request looks exactly like that, so you get `403` on
> everything. Set it to `true`, and bind to `127.0.0.1` so the listener is not
> publicly reachable.

---

## Deployment

Bare binary on a DigitalOcean droplet under systemd. **No Docker, no runtime,
no interpreter** — the artifact is one statically linked executable.

### 1. Build locally

```bash
make release                  # linux/amd64 by default
make release GOARCH=arm64     # for an ARM droplet
```

`make release` cleans, cross-compiles with `CGO_ENABLED=0 -trimpath -ldflags
"-s -w"`, then prints verification: size, SHA-256, `file(1)` output, a static
linkage check, and the binary's self-reported version.

```
==================== BUILD VERIFICATION ====================
  artifact:          bin/mcp-proxy-binary
  version:           v1.0.0
  target:            linux/amd64
  size:              13M
  sha256:            d82a3581...
  file:              ELF 64-bit LSB executable, x86-64, statically linked, stripped
  linkage:           static (no libc dependency)
============================================================
```

### 2. Prepare the droplet

```bash
ssh root@<droplet-ip>

adduser --system --group --no-create-home --shell /usr/sbin/nologin mcpproxy
mkdir -p /opt/mcp-proxy /etc/mcp-proxy
chown mcpproxy:mcpproxy /opt/mcp-proxy
chmod 750 /etc/mcp-proxy

ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw enable          # note 8080 is NOT opened: it stays on loopback
```

### 3. Ship the binary and unit

```bash
scp bin/mcp-proxy-binary          root@<droplet-ip>:/opt/mcp-proxy/
scp deploy/mcp-proxy.service      root@<droplet-ip>:/etc/systemd/system/
scp deploy/mcp-proxy.env.example  root@<droplet-ip>:/etc/mcp-proxy/mcp-proxy.env

ssh root@<droplet-ip> '
  chmod 755 /opt/mcp-proxy/mcp-proxy-binary
  chown mcpproxy:mcpproxy /opt/mcp-proxy/mcp-proxy-binary
  chmod 640 /etc/mcp-proxy/mcp-proxy.env
  chown root:mcpproxy /etc/mcp-proxy/mcp-proxy.env
  systemctl daemon-reload
  systemctl enable --now mcp-proxy
  systemctl status mcp-proxy --no-pager
'
```

Edit `/etc/mcp-proxy/mcp-proxy.env` first — at minimum set
`MCP_LISTEN_ADDR=127.0.0.1:8080` and `MCP_DISABLE_LOCALHOST_PROTECTION=true`.

Verify locally on the droplet before exposing it:

```bash
curl -s localhost:8080/healthz
```

### 4. Redeploys

```bash
make release
scp bin/mcp-proxy-binary root@<droplet-ip>:/opt/mcp-proxy/mcp-proxy-binary.new
ssh root@<droplet-ip> '
  mv /opt/mcp-proxy/mcp-proxy-binary.new /opt/mcp-proxy/mcp-proxy-binary
  chmod 755 /opt/mcp-proxy/mcp-proxy-binary
  systemctl restart mcp-proxy
'
```

`TimeoutStopSec=30s` in the unit exceeds `MCP_SHUTDOWN_GRACE_MS=15000`, so a
restart drains live streams and closes tenant pools before systemd escalates to
`SIGKILL`. Expect a restart to take a few seconds under load — that is correct
behaviour, not a hang.

---

## Connecting a domain

### 1. DNS

The service is served at **`dbmcp.hookman.online`**. At whichever provider holds
the `hookman.online` zone, add a subdomain record pointing at the droplet:

| Type | Name | Value | TTL |
|---|---|---|---|
| `A` | `dbmcp` | `<droplet-ipv4>` | 3600 |
| `AAAA` | `dbmcp` | `<droplet-ipv6>` *(optional)* | 3600 |

If `hookman.online` is managed by DigitalOcean DNS:

```bash
doctl compute domain records create hookman.online \
  --record-type A --record-name dbmcp --record-data <droplet-ipv4> --record-ttl 3600
```

Wait for propagation, then confirm it resolves to the droplet before going any
further — certbot will fail on a record that has not propagated:

```bash
dig +short dbmcp.hookman.online      # must print the droplet IP
```

### 2. nginx

```bash
ssh root@<droplet-ip>
apt-get update && apt-get install -y nginx
```

`deploy/nginx-mcp-proxy.conf` is already set to `dbmcp.hookman.online`, so it
can be copied as-is:

```bash
scp deploy/nginx-mcp-proxy.conf root@<droplet-ip>:/etc/nginx/sites-available/mcp-proxy
ssh root@<droplet-ip> '
  ln -sf /etc/nginx/sites-available/mcp-proxy /etc/nginx/sites-enabled/mcp-proxy
  rm -f /etc/nginx/sites-enabled/default
  nginx -t && systemctl reload nginx
'
```

The `ssl_certificate` paths reference files certbot has not created yet, so
`nginx -t` will fail at this point. Either run step 3 first with the HTTP-only
server block in place, or let `certbot --nginx` rewrite the file for you.

That config exists because **three nginx defaults silently break SSE**, all with
the same misleading symptom — the client connects, the request looks fine, and
no events ever arrive:

1. `proxy_buffering off` — otherwise nginx holds events until a buffer fills.
2. `proxy_set_header Connection ""` + `gzip off` — keeps chunked framing intact.
3. `proxy_read_timeout 3600s` — the 60s default severs idle MCP streams.

### 3. TLS

Credentials travel in request headers, so TLS is mandatory, not optional.

```bash
apt-get install -y certbot python3-certbot-nginx
certbot --nginx -d dbmcp.hookman.online --agree-tos -m you@example.com --redirect
systemctl list-timers | grep certbot      # auto-renewal is installed by default
```

### 4. Verify end to end

```bash
curl -s https://dbmcp.hookman.online/healthz

curl -sN -H 'x-db-username: app_reader' -H 'x-db-password: ...' \
        -H 'x-db-host: db.example.com'  -H 'x-db-port: 5432' \
        https://dbmcp.hookman.online/v1/mcp/connect
# → event: endpoint
```

If that prints the `endpoint` event, point your client at
`https://dbmcp.hookman.online/v1/mcp/connect`.

---

## Operations

### Logs

Structured JSON on stderr, captured by journald. (Nothing is ever written to
stdout — under a stdio transport that would corrupt the protocol stream, and the
discipline is kept here so the same code can serve both transports.)

```bash
journalctl -u mcp-proxy -f
journalctl -u mcp-proxy -f -o cat | jq 'select(.level=="ERROR")'
journalctl -u mcp-proxy --since "1 hour ago" -o cat | jq 'select(.fingerprint=="50e868892251b3f7")'
```

### Health

```bash
curl -s localhost:8080/healthz
# {"status":"ok","version":"v1.0.0","uptimeSeconds":3600,"activeStreams":4,"totalStreams":112}
```

`/healthz` deliberately touches **no** database. This process has none of its
own, and a tenant's unreachable server must never mark the proxy unhealthy to a
load balancer.

### Troubleshooting

| Symptom | Cause |
|---|---|
| `403 Forbidden: invalid Host header` | Set `MCP_DISABLE_LOCALHOST_PROTECTION=true` |
| Client connects, no events ever arrive | nginx `proxy_buffering` is on, or `proxy_read_timeout` is the 60s default |
| `400 invalid_db_headers` | Response body lists exactly which headers are missing or invalid |
| `could not connect to … as "user"` | Tenant DB unreachable — run `ping_database`; check the DB's firewall allows the droplet IP |
| Streams drop after ~60s | `proxy_read_timeout` in nginx |
| Restart takes ~15s | Correct: graceful drain of live streams |

### Capacity

Each live stream is one inbound socket plus up to `MCP_POOL_MAX_CONNS`
outbound ones. The binding constraint is usually the **tenant** database's
`max_connections`, not the droplet. Keep `MCP_POOL_MAX_CONNS` small (4 is
generous); `LimitNOFILE=65535` in the unit sets the local ceiling.

---

## Testing

```bash
make verify      # gofmt + go vet + go test -race ./...
```

Unit and integration coverage: tier boundaries pinned at their exact edges, CSV
round-tripped through `encoding/csv` (delimiters, quotes, embedded newlines,
NULL vs empty string), JSON column ordering and numeric precision, guard
evasion and false-positive cases, header validation and DSN escaping, credential
redaction, and the full SSE handshake including stream teardown.

### End-to-end against a real PostgreSQL

`test/e2e_test.go` drives the actual MCP protocol — SSE transport, `initialize`,
`tools/call` — against a live database. It **skips** unless `DBMCP_E2E_DSN` is
set:

```bash
# any disposable PostgreSQL will do
docker run -d --name dbmcp-pg -e POSTGRES_PASSWORD=secret \
  -e POSTGRES_DB=appdb -p 55432:5432 postgres:16-alpine

DBMCP_E2E_DSN='postgres://postgres:secret@127.0.0.1:55432/appdb?sslmode=disable' \
  go test ./test/... -v
```

Docker here is purely a convenience for a throwaway test database on a developer
machine. It is not a dependency of the service and plays no part in deployment.
