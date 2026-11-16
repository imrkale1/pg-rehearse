# pg-rehearse demo: the shop

A small but production-shaped storefront database you can break safely.

| table         | rows  | notes                                             |
|---------------|-------|---------------------------------------------------|
| `users`       | 100k  | unique email                                      |
| `products`    | 5k    | skewed popularity                                 |
| `orders`      | 1M    | `order_status` enum, nullable legacy `user_id`    |
| `order_items` | 2.5M  | FK to orders and products                         |
| `payments`    | ~980k | legacy: **no** FK to orders                       |
| `daily_revenue` (view) | | what the finance dashboard reads           |

Two Postgres 17 containers:

- **prod** (`localhost:55440`, `shop`/`shop`) — seeded on first start (~75s), with `pg_stat_statements` loaded.
- **shadow** (`localhost:55441`, `shadow`/`shadow`) — empty; pg-rehearse builds a synthetic copy here.

## 1. Start the databases

```sh
make demo-up          # = docker compose -f demo/docker-compose.yml up -d --wait
```

`--wait` returns once both are healthy, i.e. after the seed has finished
(the prod healthcheck only passes when the post-init server listens on TCP).
Follow progress with `docker compose -f demo/docker-compose.yml logs -f prod`.

## 2. Generate "app" traffic on prod

```sh
go run ./demo/traffic -dsn 'postgres://shop:shop@localhost:55440/shop?sslmode=disable' \
    -rate 300 -duration 3m
```

The mix: checkout (a transaction inserting an order, 1–4 items and a payment),
view order, order history, product page, and an **admin report** every 30s — a
~4s `REPEATABLE READ` aggregate over orders/order_items. That report is the
classic outage ingredient: any `ACCESS EXCLUSIVE` DDL queues behind it, and all
traffic queues behind the DDL.

Every statement starts with a `/* feature */` comment (`/* checkout */`,
`/* view_order */`, …), which pg_stat_statements preserves, so features can be
labelled in the profile.

## 3. Capture a profile (while traffic is running)

```sh
go run ./cmd/pg-rehearse profile \
    --dsn 'postgres://shop:shop@localhost:55440/shop?sslmode=disable' \
    --out profile.json
```

`profile.json` contains schema DDL, row counts, column statistics (with PII
columns such as `users.email` stripped of values), query rates from
pg_stat_statements and the long-running report transaction. No rows.

## 4. Rehearse a migration

```sh
go run ./cmd/pg-rehearse rehearse \
    --profile profile.json \
    --shadow-dsn 'postgres://shadow:shadow@localhost:55441/shadow?sslmode=disable' \
    --migration demo/migrations/002_index_orders_created.sql \
    --scale 0.1 --duration 30s --format markdown
```

You get the PR comment: which feature froze and for how long, what was locked,
a safer rewrite (`CREATE INDEX CONCURRENTLY`) and the result of re-rehearsing it.
Use `--format json` for machine-readable output.

Lint only (no database needed):

```sh
go run ./cmd/pg-rehearse lint demo/migrations/004_alter_type.sql
```

## The migrations

| file | what it does | expected verdict |
|------|--------------|------------------|
| `001_add_column_safe.sql` | `ADD COLUMN gift_message text` | 🟢 safe (catalog-only) — unless it queues behind the report |
| `002_index_orders_created.sql` | `CREATE INDEX` on `orders(created_at)` | 🔴 blocks checkout writes for the whole build → `CONCURRENTLY` |
| `003_fk_payments.sql` | `ADD FOREIGN KEY` without `NOT VALID` | 🔴 blocks writes on payments **and** orders while validating |
| `004_alter_type.sql` | `total TYPE numeric(14,2)` | 🔴 full table rewrite under `ACCESS EXCLUSIVE` (blocks reads too) |
| `005_not_null.sql` | `user_id SET NOT NULL` | 🟠 full scan under `ACCESS EXCLUSIVE` → `CHECK … NOT VALID` first |

Measured on the idle seeded prod (Apple Silicon, inside `BEGIN … ROLLBACK`):
002 ≈ 0.4s, 003 ≈ 1.2s, 004 ≈ 1.4s (relfilenode changes = rewrite), 005 ≈ 0.4s.
Under traffic and behind the 4s admin report these become multi-second outages.

## 5. Clean up

```sh
make demo-down        # = docker compose -f demo/docker-compose.yml down -v
```
