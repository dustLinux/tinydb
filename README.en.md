# tinydb

[English](README.en.md) | **Русский**

A small database that lives next to your program: SQLite on the inside, a REST
API on the outside. Data on disk is encrypted, the process stays under 10 MB of
RAM, and the binary builds for every Linux architecture, Termux and macOS.

It is not meant to "replace Postgres". It exists for a specific job: run a
database on a phone or in a container where there is neither memory nor desire
to install Postgres.

```sh
./webdb -data ./data          # a token is generated into ./data/token
```

You need Go (to build) or a ready binary from the
[releases](https://github.com/dustLinux/tinydb/releases/tag/v0.1.0).

[![ci](https://github.com/dustlinux/tinydb/actions/workflows/ci.yml/badge.svg)](https://github.com/dustlinux/tinydb/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dustlinux/tinydb/client-go.svg)](https://pkg.go.dev/github.com/dustlinux/tinydb/client-go)
[![license](https://img.shields.io/badge/license-BSD--3--Clause-green.svg)](LICENSE)

---

## One minute

Start the server, create a collection, write a document, read it back:

```sh
make run                          # or ./bin/webdb -addr 127.0.0.1:8099 -data ./data
TOKEN=$(cat data/token)           # generated on first start

curl -s -X POST -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"name":"items"}' \
     http://127.0.0.1:8099/v1/collections

curl -s -X PUT -H "Authorization: Bearer $TOKEN" \
     -d '{"title":"widget","price":10}' \
     'http://127.0.0.1:8099/v1/collections/items/docs/1?upsert=true'

curl -s -H "Authorization: Bearer $TOKEN" \
     'http://127.0.0.1:8099/v1/collections/items/docs?order=price:desc&limit=10'
```

Everything the server can do is in [docs/API.md](docs/API.md): collections,
documents, bulk writes, indexes, filters, read-only SQL, export/import, backup.

## The shell

If you would rather not write curl, there is `webdb-shell` — a console in the
spirit of sqlite3:

```sh
make shell
./bin/webdb-shell -addr 127.0.0.1:8099 -data ./data
```

```
tinydb> .tables
tinydb> .put items it1 {"title":"widget","price":10}
tinydb> SELECT count(*) AS n FROM docs;
tinydb> .schema items
tinydb> .stats
```

It understands `.help`, `.tables`, `.schema`, `.ls`, `.get`, `.put`, `.rm`,
`.index`, `.headers on|off`, `.mode column|list|json`, `.stats`, `.flush`,
`.export`, and read-only SQL. One-shot instead of a session:
`webdb-shell -cmd ".tables"`. Details: [docs/SHELL.md](docs/SHELL.md).

## Clients

Three languages, zero third-party dependencies.

```go
// Go — standard library only
c := webdb.New("http://127.0.0.1:8080", token)
doc, _ := c.Insert("users", map[string]any{"name": "alice"})
```

```js
// JS/TS — Node.js ≥18, browser, Deno, Bun. No build step, types included
import { Client } from 'tinydb-client';
const c = new Client('http://127.0.0.1:8080', token);
const doc = await c.insert('users', { name: 'alice' });
```

```c
/* C — own HTTP/1.1 implementation, .a/.so weigh under 30 KiB */
webdb_client c;
webdb_client_init(&c, "http://127.0.0.1:8080", token);
```

| | install | details |
|---|---|---|
| Go | `go get github.com/dustlinux/tinydb/client-go` | [docs/CLIENT-GO.md](docs/CLIENT-GO.md) |
| JS/TS | `npm install tinydb-client` | [client-js/README.md](client-js/README.md) |
| C | build from `client-c/` | [docs/CLIENT-C.md](docs/CLIENT-C.md) |

## Authentication

By default there is a static token: generated on first start, stored in
`data/token` (mode `0600`), and good for everything. When an app needs
different permissions, expiry or per-token revocation, turn on JWT:

```sh
export WEBDB_JWT_SECRET=$(openssl rand -hex 32)   # same secret on server and issuer
./webdb -data ./data -jwt-issuer tinydb -jwt-audience api

./webdb token -sub alice -ttl 24h -scp "read write" -iss tinydb -aud api
```

The server verifies the HS256 signature, `exp`/`nbf` and, when configured,
`iss`/`aud`. The algorithm comes from the server config, not from the token
header, so `alg: none` and RS/HS confusion do not work. The static token keeps
working — JWT complements it rather than replacing it.

## How it works, and why

**SQLite, but our own HTTP layer.** The server does not use `net/http`; it talks
to the socket itself (`internal/httpx`). It sounds odd, but `net/http` with its
pools, keep-alive and contexts costs more than the rest of the project put
together. The console does the opposite trick and reuses the same minimal
client: 3.5 MB instead of 6.7 MB with `net/http`.

**Writes land in memory first.** A mutation answers immediately and reaches
SQLite in batches — on a timer, on reads, when the buffer fills up, or on
`POST /v1/flush`. Reads always see your own writes, so the application never
notices the buffering. The price: `SIGKILL` may lose the last writes (never more
than one `-writeback` interval); `Ctrl-C` and `SIGTERM` always drain the buffer.

**Only ciphertext on disk.** While running, the database is decrypted into
`data/db.sqlite` (mode `0600`), but every flush/autosave writes
`data/db.sqlite.enc` — streaming AES-256-GCM with a fresh nonce per chunk — and
removes the plaintext. The key either sits next to it (`db.key`) or is replaced
by a passphrase (`-passphrase`, KDF with 100k iterations). The honest caveat: if
the key lives next to the database, a copy of the directory *is* the database;
real separation needs a passphrase or an external KMS.

**Memory is saved at every step.** The SQLite cache is capped, the Go heap
lives under `GOMEMLIMIT=2MiB`, and after every burst the server hands memory
back to the OS: `shrink_memory`, `mallopt(M_PURGE_ALL)`, `FreeOSMemory` and
`madvise` over its own code pages. On Termux/arm64 the server runs at roughly
9–10 MB RSS under load, and that is checked automatically
(`GET /v1/stats → rss_kb`).

Details, including the container format and the threat model:
[docs/DESIGN.md](docs/DESIGN.md), [docs/SECURITY.md](docs/SECURITY.md).

## Downloads

Ready binaries are in [Releases](https://github.com/dustlinux/tinydb/releases)
together with `SHA256SUMS`. Every push builds them, so the newest build is
always in [Actions](https://github.com/dustLinux/tinydb/actions).

| Platform | Architectures |
|---|---|
| Linux | `amd64`, `386`, `arm64`, `armv7`, `riscv64`, `ppc64le`, `ppc64`, `s390x`, `mips64`, `mips64le`, `mips`, `mipsle`, `loong64` |
| Termux (Android) | `arm64`, `armv7` |
| macOS | `arm64` (Apple Silicon), `amd64` (Intel) |

The `webdb-shell` console is built separately for linux/amd64, linux/arm64 and
both macOS targets. Releases also ship an **AI agent skill** as
`tinydb-agent-skill.tar.gz` ([source](skills/tinydb/SKILL.md), written in
Russian) — a short briefing on starting the server, using the console and API.

Windows is not supported and is not planned. Where distributions lack a usable
cross compiler (`ppc64` with its ELFv2, `loong64`, mips with the hard-float
ABI), the build goes through `zig cc`; version and SHA-256 are pinned in
[ci.yml](.github/workflows/ci.yml).

Build it yourself:

```sh
make build      # Termux: pkg install clang go
go build -o webdb ./cmd/webdb   # SQLite baked in, no system cgo dependencies
```

## Flags

Safe defaults: the server listens on loopback only, the token is generated for
you, auth is on.

| Flag | Default | Purpose |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | listen address; public access is explicit: `-addr :8080` |
| `-data` | `./data` | data directory (`db.sqlite`, `.enc`, `db.key`, `token`) |
| `-max-size` | `100` | disk quota, MB |
| `-autosave` | `60s` | how often to write an encrypted snapshot; `0` = only flush and shutdown |
| `-writeback` | `1s` | how often to drain the RAM buffer into SQLite; `0` = on demand only |
| `-cache-kb` | `64` | SQLite page cache per connection, KiB |
| `-go-memlimit` | `2` | Go heap limit, MB (`GOMEMLIMIT`) |
| `-max-body` | `4` | max request body, MB |
| `-max-import` | `64` | max `/v1/import` size, MB (streamed, not buffered) |
| `-buffer-bytes` / `-buffer-items` | `1 MiB` / `10000` | RAM buffer caps |
| `-key-file` | `<data>/db.key` | encryption key (32 bytes) |
| `-passphrase` | — | encrypt with a passphrase instead of a key file; prefer `WEBDB_PASSPHRASE` |
| `-token` / `-token-file` | auto | use your own API token; also `WEBDB_TOKEN` |
| `-jwt-secret` / `-jwt-secret-file` | — | accept HS256 JWTs; also `WEBDB_JWT_SECRET` |
| `-jwt-issuer` / `-jwt-audience` | — | require these JWT claims |
| `-no-auth` | off | disable authentication. Local debugging only |
| `-keep-plain` | off | keep `db.sqlite` after shutdown |
| `-cors` | off | allow CORS (for browser debugging) |

## Security

Authentication is mandatory: `Authorization: Bearer <token>` or `X-API-Key`;
everything except `/health` is closed. The token is cryptographically random,
stored with mode `0600` and never written to logs.

SQL in `/v1/query` is genuinely read-only, not just "by naming": queries run
with `PRAGMA query_only=1` and a SQLite authorizer that denies everything except
reads. So `WITH x AS (SELECT 1) DELETE FROM docs` gets a `400` and deletes
nothing (that was a real bug — found by active testing, now covered by a test).

This was checked with attacks against a live server, not by reading code: 15
auth-bypass tricks, path traversal, request smuggling, DoS, forged `.enc`, file
permissions, writes through SQL, and forged JWTs. That grew into 69 checks in
`tests/security.sh` and 22 in `tests/jwt.sh`.

What is missing: TLS (put nginx/Caddy in front), rate limiting, and protection
from local root — whoever can read the key file can read the database. Full
threat model: [docs/SECURITY.md](docs/SECURITY.md).

## Development

```sh
make check   # gofmt, vet, e2e (44), security (69), JWT (22), shell (29), clients
```

- `tests/smoke.sh` — end-to-end API test, write-back, export/import, RSS.
- `tests/security.sh` — 69 security checks.
- `tests/jwt.sh` — issuing and verifying JWTs, including `alg: none` and forged
  signatures.
- `tests/shell.sh` — console scenarios against a live server.
- `client-go`, `client-js`, `client-c` — each has its own tests against a live
  server.

CI runs all of this on Ubuntu, boots the binary on macOS, and builds every
architecture from the table above; on a `v*` tag it publishes a release with
tarballs, `SHA256SUMS` and the agent skill.

## For AI agents

The repository ships [skills/tinydb/SKILL.md](skills/tinydb/SKILL.md) — a short
briefing: starting the server, what the console can do, an endpoint table, the
usual traps (read-only SQL, write-back, limits, tokens) and how to verify
changes. Releases include it as `tinydb-agent-skill.tar.gz`.

## Read more

- [docs/API.md](docs/API.md) — every endpoint with examples.
- [docs/DESIGN.md](docs/DESIGN.md) — write-back, ciphertext and the RAM budget.
- [docs/SECURITY.md](docs/SECURITY.md) — keys, the `WEBDBENC` container, threat model.
- [PLAN.md](PLAN.md) — architecture and decisions with rationale.

## License

BSD 3-Clause, see [LICENSE](LICENSE).
