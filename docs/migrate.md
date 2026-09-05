# Migrations

Numbered `.up.sql` / `.down.sql` pairs, a runner that applies them in one
transaction each, and a ledger table that records what was applied and whether it
has since changed.

```
migrations/
  0001_create_users.up.sql
  0001_create_users.down.sql
  0002_add_email_index.up.sql
  0002_add_email_index.down.sql
```

```go
import (
    "embed"
    "github.com/nelthaarion/breeze/v2/migrate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

sub, _ := fs.Sub(migrationsFS, "migrations")
runner := migrate.New(db, sub)

if err := runner.Up(context.Background()); err != nil {
    log.Fatal(err)
}
```

```bash
breeze makemigration create_users   # writes the .up.sql/.down.sql pair
breeze migrate up                   # apply everything pending
breeze migrate down 1               # roll back the last one
breeze migrate status               # what is applied, and what drifted
```

## File naming

```
<version>_<slug>.up.sql
<version>_<slug>.down.sql
```

`version` is four or more digits, `slug` is lowercase with underscores. The pattern
is enforced (`^(\d{4,})_([a-z0-9_]+)\.(up|down)\.sql$`), and discovery **fails**
rather than warns on:

- an `.up.sql` with no matching `.down.sql`, or the reverse
- two migrations sharing a version number

Both are refused because the failure they cause otherwise is worse than a startup
error. A missing `.down.sql` is a migration that cannot be rolled back, discovered
at the moment you need to roll it back. Two migrations with the same version have
an order that depends on directory iteration, so the same repository applies them
differently on two machines.

Zero-padding is not required by the regex but is required in practice for the
`ls`-order to match the apply order — the generator always writes four digits.

## `Up`, `Down`, `Status`

```go
err := runner.Up(ctx)              // every pending migration, ascending
err := runner.Down(ctx, 1)         // the last n, descending
entries, err := runner.Status(ctx) // per-migration state
```

Each migration runs in **its own transaction**, not one transaction around the
batch. `Up` stops at the first failure and does not attempt the rest, so a failed
run leaves the database at the last migration that succeeded — a state the ledger
records and `status` can show. One big transaction would be cleaner in theory and
unusable in practice: several databases cannot run DDL transactionally, and a
partial rollback of half a schema change is not recoverable by looking at it.

`Status` returns a `StatusEntry` per migration:

| Field | Meaning |
|---|---|
| `Version`, `Name` | from the filename |
| `Applied` | is it in the ledger |
| `AppliedAt` | when, or nil |
| `ChecksumMismatch` | the file changed after it was applied |

## The ledger

One table, created on first use:

```sql
CREATE TABLE IF NOT EXISTS breeze_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT      NOT NULL,
    checksum   TEXT      NOT NULL,
    applied_at TIMESTAMP NOT NULL
)
```

ANSI SQL that works on both Postgres and SQLite, with `?` placeholders —
`database/sql`'s default convention. A driver that requires numbered placeholders
(lib/pq for Postgres) needs a wrapper that rewrites `?` to `$N`, such as pgx's
stdlib adapter with its query rewriter enabled. That is the price of the `migrate`
package having no hard dependency on any single driver.

### Checksums

`checksum` is the SHA-256 of the `.up.sql` content at the time it was applied.
`Status` recomputes it and sets `ChecksumMismatch` when the file has changed since.

This is a *report*, not an error. Editing an applied migration is sometimes the
right thing — a typo in a comment — and sometimes the beginning of an incident,
where a colleague's database ran different SQL than yours. The runner cannot tell
which, so it tells you and lets you decide.

### The lock

Concurrent runners are serialised by a **sentinel row** in the same ledger table,
at version `-1`:

```
acquireLock:  INSERT version = -1   → the primary key makes it mutually exclusive
releaseLock:  DELETE version = -1
```

A row rather than `pg_advisory_lock` because the advisory-lock functions are
Postgres-specific, and this package works against any `database/sql` driver. The
primary key is what provides the exclusion — a second runner's insert violates it.

`-1` because discovered versions are parsed from filenames as unsigned digits, so a
negative number can never collide with a real migration.

Two consequences worth knowing:

- **Every ledger read excludes it.** Without that filter `Down` — which acquires the
  lock before reading the ledger — saw a migration numbered `-1`, found no file for
  it, and failed with `applied migration -1 not found in migration files` on every
  single run.
- **A failed insert is reported as "another migration is running".** The portable
  API cannot distinguish a constraint violation from any other insert failure, so
  a genuinely corrupt table reports the same message. That is stated in the error
  text rather than hidden: *"another migration is running (or lock table is
  corrupted); wait and try again"*.

`releaseLock` warns to stderr rather than failing. By then the migration is done,
and a stuck sentinel row blocks the *next* run, not this one — turning a completed
migration into a returned error would be the more misleading outcome.

## Statement splitting

Migration files are split on semicolons, with basic quote handling. This is
deliberately naive and suits simple DDL. A file containing a `CREATE FUNCTION` body,
a `BEGIN … END` block, or a semicolon inside a dollar-quoted string will split
wrongly — put those in their own migration and keep it to one statement.

## Concurrency

`Runner` is **not safe for concurrent use** by multiple goroutines. It is safe
against concurrent *processes* — that is what the lock is for.

The distinction matters for a deployment that runs migrations from every replica on
startup: that works, one wins the lock and the rest wait. Two goroutines in one
process sharing a `Runner` do not, and there is no reason to.

## Diagnostics

The `migrate` probe reports the runner's configuration and the last operation:

```bash
curl localhost:3000/dashboard/api/diagnostics?subsystem=migrate
```

| Detail | Meaning |
|---|---|
| `database`, `migrations` | whether `DB` and `FS` are set |
| `connections` | `database/sql` pool stats: open, in use, idle, wait count |
| `last_run` | operation, when, count, duration, error |

`record` is called from a `defer` in `Up`, `Down` and `Status`, so a panic or an
early return still records — an operation that failed is the one worth reporting.

One case the probe handles specifically: a recorded run with **no registered
runner** means someone built a `Runner` as a struct literal rather than through
`migrate.New`. The probe reports the run anyway, with a note saying the
configuration is unavailable and why. Reporting the run is more useful than
reporting the absence of the thing that performed it.

Reading `DB.Stats()` is a mutex-guarded struct copy and touches no connection, which
is what makes it acceptable in a probe.

## CLI

`breeze migrate` shells out to `cmd/migrate` in your project — the program
`breeze add migrator` generates. The CLI does not embed a database driver, so the
project chooses it:

```
breeze migrate up
breeze migrate down [n]
breeze migrate status
```

Flags are forwarded to the project's runner.

`breeze makemigration <Name>` writes the pair. It converts `CreateUsersTable` to
`create_users_table` with acronym runs kept together, so `AddHTTPCache` becomes
`add_http_cache` and not `add_h_t_t_p_cache`.

> This package does not write migration files. It discovers and runs the ones
> already on disk; the generator writes them. Two functions naming the same artefact
> would be a problem, so there is only one — see the note at
> [`../migrate/parse.go`](../migrate/parse.go) for what was removed and why.

## See also

- [`../README.md`](../README.md) — the tour's CLI section
- [`diag.md`](./diag.md) — the `migrate` probe
- [`repository-structure.md`](./repository-structure.md) — where this package sits


