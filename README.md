# pg-rehearse

**Catch the database outage on the pull request, not in production.**

A Postgres schema change can look harmless and still take down your site. Adding
a column, creating an index, adding a foreign key — each one briefly locks a
table, and if an unlucky long-running query is holding that table at the time,
every request queues behind the migration. A one-line change becomes a
multi-second outage. Teams usually discover this *in production*, because the
danger isn't in the SQL — it's in how big the table is, how much traffic it's
taking, and what else is running at that moment.

pg-rehearse is a dress rehearsal. Before a migration ships, it:

1. **Profiles** the production database using only its statistics — table sizes,
   query shapes, rates — and **never reads a single customer row**.
2. **Builds a synthetic copy** that has production's shape and size, filled with
   fake data drawn from those statistics.
3. **Replays realistic traffic** against the copy at production rate, applies the
   migration mid-traffic, and **measures how long each feature stalls**.
4. **Suggests a safer rewrite**, applies it to a fresh copy, and **re-tests it**
   to prove the stall is gone.

Then it posts the verdict on the pull request:

> 🔴 This migration would have frozen **checkout** for **6.5s**.
> ✅ Re-tested the safer version: checkout max 100ms, no stalls.

---

## See it in action

The demo is a storefront database — 100k users, 1M orders, 2.5M order items —
with a traffic generator that simulates checkouts, order lookups, and a slow
admin report running every 30s (the classic outage ingredient).

```bash
# 1. Start production-like + empty shadow databases (seeds ~1M orders, ~60s)
make demo-up

# 2. Generate realistic traffic against "production"
go run ./demo/traffic -rate 300 -duration 3m &

# 3. Capture a privacy-safe profile (reads statistics only)
./bin/pg-rehearse profile \
  --dsn postgres://shop:shop@localhost:55440/shop?sslmode=disable \
  --out shop.profile.json

# 4. Rehearse a migration
./bin/pg-rehearse rehearse \
  --profile shop.profile.json \
  --shadow-dsn postgres://shadow:shadow@localhost:55441/shadow?sslmode=disable \
  --migration demo/migrations/001_add_column_safe.sql
```

`demo/migrations/001_add_column_safe.sql` is a single `ALTER TABLE orders ADD
COLUMN gift_message text` — 7ms in isolation. Under traffic, with the admin
report holding `orders`, pg-rehearse measures it freezing **every feature for
6.5 seconds**, then proves the safer version (a short-`lock_timeout` retry loop)
completes with a **100ms** worst case and no stall. A full example comment is in
[`demo/example-report.md`](demo/example-report.md).

---

## How each piece works

| Package | Responsibility |
|---|---|
| [`internal/profile`](internal/profile) | Reads `pg_class`, `pg_stats`, `pg_stat_statements`, `pg_stat_activity` to capture table sizes, column distributions, query rates and long-running transactions. Regenerates the schema DDL from the catalogs (no `pg_dump`). Strips PII: sensitive columns keep only aggregate numbers, never sample values. |
| [`internal/synth`](internal/synth) | Rebuilds the schema on an empty database and fills every table with synthetic rows matching production's row counts and per-column distributions, in foreign-key order, via `COPY`. ~450k rows/sec. |
| [`internal/locks`](internal/locks) | Runs the migration inside `BEGIN … ROLLBACK`, reads `pg_locks` to report exactly which tables it locks and whether reads/writes are blocked, and detects full-table rewrites (`relfilenode` change). Plus 13 static lint rules. |
| [`internal/fixer`](internal/fixer) | Rewrites risky migrations into safer equivalents: `CREATE INDEX CONCURRENTLY`, foreign keys / checks added `NOT VALID` then `VALIDATE` separately, `SET NOT NULL` via a validated check, and a short-`lock_timeout` **retry loop** around every blocking statement. |
| [`internal/load`](internal/load) | Replays the profiled query mix open-loop (Poisson arrivals, so stalls surface instead of hiding), samples real parameter values from the copy, and reproduces lock-queue outages. |
| [`internal/report`](internal/report) | Renders the Markdown PR comment and a JSON result. |
| [`internal/rehearse`](internal/rehearse) | Orchestrates: build/cache a synthetic template, clone per run, drive traffic, apply the migration under load, measure stalls, suggest a fix and re-test it. |

### Why the fake traffic matters

A linter can read the SQL and tell you `CREATE INDEX` locks the table. What it
*can't* tell you is whether that lock becomes an outage — that depends on table
size, traffic, and concurrent transactions, none of which are in the code. The
worst outages come from **lock queuing**: the migration waits behind one slow
query, and every new request waits behind the migration. pg-rehearse is built to
reproduce exactly that, so it can put a number on it.

### Why no production data

The synthetic copy is built from statistics, not rows, so there's nothing
sensitive to leak and compliance teams don't have to sign off on a data copy.
Column value samples from `pg_stats` are dropped for anything that looks like PII
(email, name, token, …) or any wide free-text column — only aggregate numbers
(null fraction, distinct count, histogram shape) are kept.

---

## Observability

The orchestrator is instrumented with **OpenTelemetry**: it emits a span per
rehearsal phase (`rehearse.run` → `rehearse.once` → `rehearse.analyze_locks`,
`rehearse.apply_migration`) and metrics for the numbers that matter —
`pgr.migration.duration`, `pgr.rehearsal.max_stall`, and a
`pgr.rehearsal.verdicts` counter by phase and severity. Export is opt-in over
OTLP, so it works with the **Datadog Agent**, an OpenTelemetry Collector, Jaeger,
or any OTLP backend, and is a no-op (zero cost) when unconfigured:

```bash
# Datadog Agent / OTel Collector listening on :4318
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_RESOURCE_ATTRIBUTES=deployment.environment=ci,service.version=1.0.0
./bin/pg-rehearse rehearse --profile shop.profile.json --shadow-dsn ... --migration m.sql
```

Traces show where a rehearsal spends its time (building the copy vs. analyzing
locks vs. driving traffic); the verdict counter and stall histogram let you
track, across many PRs, how often migrations would have caused an outage.

## Use it in CI

### Fail the build on a stall budget

`--fail-on` gates on the verdict. To gate on a hard latency budget instead, pass
`--max-stall`: `rehearse` exits with code 2 when any feature stalls longer than
the limit while the original migration runs.

```bash
./bin/pg-rehearse rehearse --profile shop.profile.json --shadow-dsn ... \
  --migration m.sql --max-stall 500ms
# pg-rehearse: max stall 6500ms exceeds --max-stall 500ms   (exit 2)
```

In the GitHub Action, set the `max-stall` input (for example `max-stall: 500ms`).

[`action.yml`](action.yml) is a composite GitHub Action, and
[`.github/workflows/example-pr-rehearsal.yml`](.github/workflows/example-pr-rehearsal.yml)
shows wiring it up: on a PR that touches migration files, it spins up an
ephemeral shadow Postgres, rehearses each changed migration against a committed
profile, and posts a single sticky comment with the verdict.

---

## Build and test

```bash
make build            # -> bin/pg-rehearse
make test             # unit tests (no database needed)
make test-integration # starts a throwaway Postgres and runs the full suite
```

Integration tests are gated on `PGR_TEST_DSN` and skip when it is unset.

---

## Honest limitations

- **Timing-dependent, not a guarantee.** The result is a strong signal, not a
  proof: the copy runs on different hardware, and fake data won't match
  production's exact hot rows or cache state. The PR comment says so.
- A migration that forces a **full-table rewrite** (`ALTER COLUMN … TYPE`) holds
  its lock for the whole rewrite; a retry loop caps the *wait* for the lock, not
  the rewrite itself, so pg-rehearse reports it honestly instead of claiming a fix.
- `pg_stat_statements` must be enabled on the production database to profile
  query rates.
- Synthetic data models common distributions, not every constraint (exclusion
  constraints, complex partition bounds, overlapping composite FKs are not
  fully modelled).

---

## Why I built it

Database migrations are one of the most common causes of self-inflicted
downtime, and the standard tools (linters) stop at "this looks risky." I wanted
to see whether you could go one step further and actually *measure* the blast
radius under realistic load, then prove a fix — turning "this looks risky" into
"this would freeze checkout for 6.5s, and here's a version I tested that doesn't."
