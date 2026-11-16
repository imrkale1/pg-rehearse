<!-- pg-rehearse:report -->
### 🔴 This migration would have frozen **order history** for **6.5s**

ran in 6.2s · rehearsed under 603 queries/s across 11 query shapes, 1 long-running transaction(s)

| Feature | Normal p99 | Worst during migration | Stall | With safer version |
|---|--:|--:|--:|--:|
| order history | 5.5ms | 6.2s | **6.5s** | 100ms |
| checkout | 2.3ms | 6.2s | **6.5s** | 102ms |
| view order | 2.3ms | 6.2s | **6.5s** | 101ms |
| product page | 4.5ms | 6s | **6.3s** | 38ms |

**Lock footprint:** Locks `orders` (blocks reads and writes).

**Findings**

- 🟠 `missing-lock-timeout` — The migration takes locks that block application queries but never sets lock_timeout. If the lock is not granted immediately (e.g. a long-running report holds a conflicting lock), the DDL waits in the lock queue and every new query on the table queues behind it: an outage that lasts as long as the longest running transaction. _Start the migration with SET lock_timeout = '2s'; so it fails fast instead of blocking everyone, and retry it._

✅ Re-tested: order history max 100ms, no stalls (migration took 6.3s).

<details>
<summary>Suggested safer migration</summary>

- **missing-lock-timeout** — Take the lock in short attempts (SET LOCAL lock_timeout = '100ms' in a retry loop) instead of waiting indefinitely: each attempt blocks application queries for at most 100ms, and between attempts a conflicting transaction (e.g. a long-running report) can commit, so a brief DDL lock never turns into an outage.

```sql
-- missing-lock-timeout
-- Safe: adding a nullable column without a default is a catalog-only change.
-- It still takes an ACCESS EXCLUSIVE lock, but only for a few milliseconds
-- (unless it has to queue behind a long transaction such as the admin report).
DO $pgr$
BEGIN
  -- pg-rehearse: acquire the lock in short attempts so a slow concurrent
  -- transaction cannot turn this DDL into an outage (see retryDesc).
  FOR attempt IN 1..25 LOOP
    BEGIN
      SET LOCAL lock_timeout = '100ms';
      -- Safe: adding a nullable column without a default is a catalog-only change.
-- It still takes an ACCESS EXCLUSIVE lock, but only for a few milliseconds
-- (unless it has to queue behind a long transaction such as the admin report).
ALTER TABLE orders ADD COLUMN gift_message text;
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      RAISE NOTICE 'pg-rehearse: lock busy, attempt % of 25', attempt;
      PERFORM pg_sleep(2);
    END;
  END LOOP;
  RAISE EXCEPTION 'pg-rehearse: could not acquire lock after 25 attempts';
END
$pgr$;
```

</details>

---
<sub>🎭 <b>pg-rehearse</b> ran this migration against a synthetic, production-shaped copy of your database while replaying realistic traffic. No production data was read or modified.</sub>
