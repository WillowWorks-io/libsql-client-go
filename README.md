# libsql-client-go

**A maintained, pure-Go libSQL/Hrana client for Turso Cloud.** No cgo, no native
libraries, builds as a static binary on distroless.

This is a fork of [`tursodatabase/libsql-client-go`](https://github.com/tursodatabase/libsql-client-go),
which upstream deprecated in favour of [`go-libsql`](https://github.com/tursodatabase/go-libsql).
That successor requires cgo, which is a hard blocker for anyone shipping
`CGO_ENABLED=0` static binaries, and buys nothing for a remote-only client that
never touches an embedded replica. Upstream issue
[#141](https://github.com/tursodatabase/libsql-client-go/issues/141) is a user
making exactly that objection.

So this fork exists to keep the pure-Go path alive, and to fix the bugs upstream
is no longer accepting patches for.

## What is fixed here

Two optional `database/sql` interfaces the driver never advertised. Both
omissions appear accidental rather than considered: `driver.Validator` has never
appeared anywhere in upstream's history, and the PR that added ping states in its
first line that the methods "satisfy the sql driver interface", which they do not.

**`driver.Validator` — the connection pool recycled dead connections.**
On the remote HTTP transport the server closes the Hrana stream after each
statement, so a connection is spent the moment its query returns. Without a
Validator, `database/sql` assumes every returned connection is healthy and pools
it; the next caller fails with `stream is closed: driver: bad connection`.

`DB.retry` hides this most of the time — two attempts with `cachedOrNewConn`,
then one with `alwaysNewConn` that opens something fresh. It stops hiding it
once the pool saturates: past `MaxOpenConns`, `alwaysNewConn` cannot open
anything, so it queues and is handed the next connection another goroutine
returns, which is also spent. All three attempts fail and the error reaches the
caller. **The symptom is intermittent 500s on your widest fan-out**, at a
concurrency level far below anything Turso itself strains at — the service
benchmarks clean to 64 concurrent at 422 q/s with zero errors. It is easy to
misread as a Turso limit. It is not one.

**`driver.Pinger` — `DB.Ping()` never reached the server.**
The interface requires exactly `Ping(ctx context.Context) error`. The driver
offered `Ping() error` and `PingContext(ctx) error`, matching neither, so
`database/sql` detected no Pinger and `DB.Ping()` returned as soon as it could
take a connection from the pool — verifying nothing over the network. Measured:
a ping returned in **2µs** where a real query took **48.8ms**. If you built a
keepalive on `DB.Ping()` to hold a suspending instance awake, it was doing
nothing.

## Install

```
go get github.com/WillowWorks-io/libsql-client-go
```

```go
import _ "github.com/WillowWorks-io/libsql-client-go/libsql"

db, err := sql.Open("libsql", "libsql://your-db.turso.io?authToken=...")
```

Migrating from upstream is a path swap and nothing else — the package layout and
API are unchanged:

```
s|github.com/tursodatabase/libsql-client-go|github.com/WillowWorks-io/libsql-client-go|
```

## Credentials: URL or option, and who wins

The auth token may come from either the connection URL or an option:

```go
// both of these work
libsql.NewConnector("libsql://db.turso.io?authToken=" + tok)
libsql.NewConnector("libsql://db.turso.io", libsql.WithAuthToken(tok))
```

Upstream `NewConnector` **rejected** the first form outright — while
`Driver.Open`, the other entry point to the same driver, accepted it. Since
Turso issues connection strings with `?authToken=` in them, the rejected shape
was the common one, and migrating from `sql.Open` to `NewConnector` failed at
startup. Both are accepted here.

**If both are given and they differ, the option wins.** A connection string is
usually handed to a program by its platform — an injected secret, an env var —
so the option is the half the caller can actually change; making them edit the
URL first would defeat the point of having an option.

The conflict is **logged at WARN via `slog`**, because the other way to end up
here is a half-finished credential rotation, and that should not be discovered
later as an auth failure. The values are never logged: a warning that printed
them would turn a config smell into a secret in your log store.

The same precedence applies to `?tls=` and `WithTls`. A TLS setting merely
*implied* by the URL scheme is not a conflict and is overridden quietly.

## Never log the connection URL

Turso embeds the auth token in the connection string, so the natural thing to
write is the dangerous thing:

```go
return fmt.Errorf("opening database %q: %w", databaseURL, err) // leaks a live credential
```

That line writes a working token into whatever collects the error — log
aggregator, error tracker, CI transcript, a terminal someone screenshots. It is
easy to miss because the URL reads as configuration rather than a secret.

Use `RedactURL`:

```go
return fmt.Errorf("opening database %q: %w", libsql.RedactURL(databaseURL), err)
```

It replaces `authToken` / `auth_token` / `jwt` and any userinfo password with
`REDACTED`, keeps everything else so the message stays diagnostic, and refuses
to return input it could not parse. As a backstop it scans the result for
anything JWT-shaped — a token under an unrecognised parameter name — and
withholds the URL entirely rather than hand it back.

## Optional: keep a suspending instance warm

A Turso instance suspends when nothing queries it, and waking costs about a
second and a half — measured 49ms after 30s idle, 1.83s after 60s. The first
request after a quiet stretch eats that, for reasons that have nothing to do
with the query being run.

`WithKeepAlive` holds the instance awake, and is **adaptive**: it issues a
statement only when nothing else has, so it stays silent while real traffic is
already keeping the database warm. On a usage-billed service, a blind ticker is
waste you pay for.

```go
c, err := libsql.NewConnector(dsn, libsql.WithKeepAlive(25*time.Second))
if err != nil { ... }

db := sql.OpenDB(c)
defer db.Close() // also stops the keepalive
```

Pick an interval under the suspend threshold, which measured between 30 and 60
seconds; 25s leaves margin for a late tick.

It is entirely **opt-in**. A connector built without it behaves exactly as
before: no goroutine, no queries of its own. It is also ignored for `file:`
databases and for a non-positive interval, so the same options can be passed for
a local and a remote database without branching.

Note the activity signal is taken at the connector, so it sees every query the
application makes without any call site reporting in — and it is per connector,
so one busy database will not keep another's instance from being tickled.

## One more thing worth knowing

`http.Transport.MaxIdleConnsPerHost` defaults to **2**, and this driver sends
every query to one host through `http.DefaultClient`. Past two concurrent
queries the surplus connections cannot be kept idle, so each subsequent query
pays a full TCP+TLS handshake (~200ms) instead of one round trip (~50ms). This
lands hardest on fan-out workloads. Raising it is worth more than any
`database/sql` pool setting:

```go
if t, ok := http.DefaultTransport.(*http.Transport); ok {
    t.MaxIdleConnsPerHost = 32
}
```

Measured effect: p50 69.7ms → 53.2ms at 8-wide, 193ms → 116.5ms at 32-wide.

## Scope

Remote Turso Cloud over HTTP is what this fork supports and tests. The `ws://`
transport is inherited but effectively dead — the driver negotiates only
`hrana1` and Turso now rejects it (`expected handshake response status code 101
but got 400`). It is retained for self-hosted `sqld` users; if you rely on it,
please open an issue, otherwise it will likely be removed.

Embedded replicas are out of scope — that is what `go-libsql` and its cgo
bindings are for.

## Status and maintenance

**Actively maintained.** This is not a drive-by fork.

Turso is production infrastructure for everything WillowWorks builds, so this
client is a dependency we run in anger rather than one we publish and forget.
Bugs that affect us get fixed here first, and CI runs on every push against a
real `libsql-server` container.

What that commits to:

- **Correctness fixes and Turso Cloud compatibility.** If the Hrana wire
  protocol shifts and this client breaks, fixing it is not optional for us.
- **Issues get a reply.** Not necessarily a fix, but an honest answer about
  whether it is in scope and whether anyone is working on it.
- **Semver, and no surprise breaks.** The API is upstream's; keeping it that way
  is a feature, since it makes migrating a module path swap.

What it does not commit to:

- **Embedded replicas.** Out of scope, permanently. That needs cgo and
  [`go-libsql`](https://github.com/tursodatabase/go-libsql) already does it.
- **Feature parity with whatever Turso ships next.** If Turso Cloud moves off
  Hrana entirely, this client's job ends and the README will say so plainly
  rather than quietly rotting.

If you are depending on this and something is missing, open an issue -- knowing
who is out there is what tells us where to spend effort.

## Credit

All original work is by the libSQL authors and 30 contributors, MIT licensed,
full history preserved. See [LICENSE](./LICENSE).

---


<p align="center">
  <a href="https://docs.turso.tech/sdk/go/quickstart">
    <img alt="Turso + Go cover" src="https://github.com/tursodatabase/libsql-client-go/assets/950181/d1bf85da-b906-4cbd-8b65-533b1307b4ff" width="1000">
    <h3 align="center">Turso + Go</h3>
  </a>
</p>

<p align="center">
  SQLite for Production. Powered by <a href="https://turso.tech/libsql">libSQL</a>.
</p>

<p align="center">
  <a href="https://turso.tech"><strong>Turso</strong></a> ·
  <a href="https://docs.turso.tech/quickstart"><strong>Quickstart</strong></a> ·
  <a href="/examples"><strong>Examples</strong></a> ·
  <a href="https://docs.turso.tech"><strong>Docs</strong></a> ·
  <a href="https://discord.com/invite/4B5D7hYwub"><strong>Discord</strong></a> ·
  <a href="https://blog.turso.tech/"><strong>Blog &amp; Tutorials</strong></a>
</p>

<p align="center">
  <a href="https://discord.com/invite/4B5D7hYwub">
    <img src="https://dcbadge.vercel.app/api/server/4B5D7hYwub?style=flat" alt="discord activity" title="join us on discord" />
  </a>
    <a href="https://www.phorm.ai/query?projectId=3c9a471f-4a47-469f-81f6-4ea1ff9ab418"><img src="https://img.shields.io/badge/Phorm-Ask_AI-%23F2777A.svg?&logo=data:image/svg+xml;base64,PHN2ZyB3aWR0aD0iNSIgaGVpZ2h0PSI0IiBmaWxsPSJub25lIiB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciPgogIDxwYXRoIGQ9Ik00LjQzIDEuODgyYTEuNDQgMS40NCAwIDAgMS0uMDk4LjQyNmMtLjA1LjEyMy0uMTE1LjIzLS4xOTIuMzIyLS4wNzUuMDktLjE2LjE2NS0uMjU1LjIyNmExLjM1MyAxLjM1MyAwIDAgMS0uNTk1LjIxMmMtLjA5OS4wMTItLjE5Mi4wMTQtLjI3OS4wMDZsLTEuNTkzLS4xNHYtLjQwNmgxLjY1OGMuMDkuMDAxLjE3LS4xNjkuMjQ2LS4xOTFhLjYwMy42MDMgMCAwIDAgLjItLjEwNi41MjkuNTI5IDAgMCAwIC4xMzgtLjE3LjY1NC42NTQgMCAwIDAgLjA2NS0uMjRsLjAyOC0uMzJhLjkzLjkzIDAgMCAwLS4wMzYtLjI0OS41NjcuNTY3IDAgMCAwLS4xMDMtLjIuNTAyLjUwMiAwIDAgMC0uMTY4LS4xMzguNjA4LjYwOCAwIDAgMC0uMjQtLjA2N0wyLjQzNy43MjkgMS42MjUuNjcxYS4zMjIuMzIyIDAgMCAwLS4yMzIuMDU4LjM3NS4zNzUgMCAwIDAtLjExNi4yMzJsLS4xMTYgMS40NS0uMDU4LjY5Ny0uMDU4Ljc1NEwuNzA1IDRsLS4zNTctLjA3OUwuNjAyLjkwNkMuNjE3LjcyNi42NjMuNTc0LjczOS40NTRhLjk1OC45NTggMCAwIDEgLjI3NC0uMjg1Ljk3MS45NzEgMCAwIDEgLjMzNy0uMTRjLjExOS0uMDI2LjIyNy0uMDM0LjMyNS0uMDI2TDMuMjMyLjE2Yy4xNTkuMDE0LjMzNi4wMy40NTkuMDgyYTEuMTczIDEuMTczIDAgMCAxIC41NDUuNDQ3Yy4wNi4wOTQuMTA5LjE5Mi4xNDQuMjkzYTEuMzkyIDEuMzkyIDAgMCAxIC4wNzguNThsLS4wMjkuMzJaIiBmaWxsPSIjRjI3NzdBIi8+CiAgPHBhdGggZD0iTTQuMDgyIDIuMDA3YTEuNDU1IDEuNDU1IDAgMCAxLS4wOTguNDI3Yy0uMDUuMTI0LS4xMTQuMjMyLS4xOTIuMzI0YTEuMTMgMS4xMyAwIDAgMS0uMjU0LjIyNyAxLjM1MyAxLjM1MyAwIDAgMS0uNTk1LjIxNGMtLjEuMDEyLS4xOTMuMDE0LS4yOC4wMDZsLTEuNTYtLjEwOC4wMzQtLjQwNi4wMy0uMzQ4IDEuNTU5LjE1NGMuMDkgMCAuMTczLS4wMS4yNDgtLjAzM2EuNjAzLjYwMyAwIDAgMCAuMi0uMTA2LjUzMi41MzIgMCAwIDAgLjEzOS0uMTcyLjY2LjY2IDAgMCAwIC4wNjQtLjI0MWwuMDI5LS4zMjFhLjk0Ljk0IDAgMCAwLS4wMzYtLjI1LjU3LjU3IDAgMCAwLS4xMDMtLjIwMi41MDIuNTAyIDAgMCAwLS4xNjgtLjEzOC42MDUuNjA1IDAgMCAwLS4yNC0uMDY3TDEuMjczLjgyN2MtLjA5NC0uMDA4LS4xNjguMDEtLjIyMS4wNTUtLjA1My4wNDUtLjA4NC4xMTQtLjA5Mi4yMDZMLjcwNSA0IDAgMy45MzhsLjI1NS0yLjkxMUExLjAxIDEuMDEgMCAwIDEgLjM5My41NzIuOTYyLjk2MiAwIDAgMSAuNjY2LjI4NmEuOTcuOTcgMCAwIDEgLjMzOC0uMTRDMS4xMjIuMTIgMS4yMy4xMSAxLjMyOC4xMTlsMS41OTMuMTRjLjE2LjAxNC4zLjA0Ny40MjMuMWExLjE3IDEuMTcgMCAwIDEgLjU0NS40NDhjLjA2MS4wOTUuMTA5LjE5My4xNDQuMjk1YTEuNDA2IDEuNDA2IDAgMCAxIC4wNzcuNTgzbC0uMDI4LjMyMloiIGZpbGw9IndoaXRlIi8+CiAgPHBhdGggZD0iTTQuMDgyIDIuMDA3YTEuNDU1IDEuNDU1IDAgMCAxLS4wOTguNDI3Yy0uMDUuMTI0LS4xMTQuMjMyLS4xOTIuMzI0YTEuMTMgMS4xMyAwIDAgMS0uMjU0LjIyNyAxLjM1MyAxLjM1MyAwIDAgMS0uNTk1LjIxNGMtLjEuMDEyLS4xOTMuMDE0LS4yOC4wMDZsLTEuNTYtLjEwOC4wMzQtLjQwNi4wMy0uMzQ4IDEuNTU5LjE1NGMuMDkgMCAuMTczLS4wMS4yNDgtLjAzM2EuNjAzLjYwMyAwIDAgMCAuMi0uMTA2LjUzMi41MzIgMCAwIDAgLjEzOS0uMTcyLjY2LjY2IDAgMCAwIC4wNjQtLjI0MWwuMDI5LS4zMjFhLjk0Ljk0IDAgMCAwLS4wMzYtLjI1LjU3LjU3IDAgMCAwLS4xMDMtLjIwMi41MDIuNTAyIDAgMCAwLS4xNjgtLjEzOC42MDUuNjA1IDAgMCAwLS4yNC0uMDY3TDEuMjczLjgyN2MtLjA5NC0uMDA4LS4xNjguMDEtLjIyMS4wNTUtLjA1My4wNDUtLjA4NC4xMTQtLjA5Mi4yMDZMLjcwNSA0IDAgMy45MzhsLjI1NS0yLjkxMUExLjAxIDEuMDEgMCAwIDEgLjM5My41NzIuOTYyLjk2MiAwIDAgMSAuNjY2LjI4NmEuOTcuOTcgMCAwIDEgLjMzOC0uMTRDMS4xMjIuMTIgMS4yMy4xMSAxLjMyOC4xMTlsMS41OTMuMTRjLjE2LjAxNC4zLjA0Ny40MjMuMWExLjE3IDEuMTcgMCAwIDEgLjU0NS40NDhjLjA2MS4wOTUuMTA5LjE5My4xNDQuMjk1YTEuNDA2IDEuNDA2IDAgMCAxIC4wNzcuNTgzbC0uMDI4LjMyMloiIGZpbGw9IndoaXRlIi8+Cjwvc3ZnPgo=" alt="phorm.ai">
  </a>
</p>

---

## Documentation

1. [Turso Quickstart](https://docs.turso.tech/quickstart) &mdash; Learn how create and connect your first database.
2. [SDK Quickstart](https://docs.turso.tech/sdk/go/quickstart) &mdash; Learn how to install and execute queries using the libSQL client.

### What is Turso?

[Turso](https://turso.tech) is a SQLite-compatible database built on [libSQL](https://docs.turso.tech/libsql), the Open Contribution fork of SQLite. It enables scaling to hundreds of thousands of databases per organization and supports replication to any location, including your own servers, for microsecond-latency access.

Learn more about what you can do with Turso:

- [Embedded Replicas](https://docs.turso.tech/features/embedded-replicas)
- [Platform API](https://docs.turso.tech/features/platform-api)
- [Data Edge](https://docs.turso.tech/features/data-edge)
- [Branching](https://docs.turso.tech/features/branching)
- [Point-in-Time Recovery](https://docs.turso.tech/features/point-in-time-recovery)
- [Scale to Zero](https://docs.turso.tech/features/scale-to-zero)
