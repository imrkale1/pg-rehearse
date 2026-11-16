<!-- pg-rehearse:report -->
### 🔴 This migration would have frozen **checkout** for **1.6s**

ran in 1.5s · rehearsed under 618 queries/s across 11 query shapes, 1 long-running transaction(s)

| Feature | Normal p99 | Worst during migration | Stall | With safer version |
|---|--:|--:|--:|--:|
| checkout | 2.9ms | 1.5s | **1.6s** | 1.5s, stalled 1.5s |
| order history | 11ms | 1.5s | **1.6s** | 1.5s, stalled 1.5s |
| view order | 2.4ms | 1.5s | **1.5s** | 1.5s, stalled 1.5s |
| product page | 2.9ms | 1.3s | **1.4s** | 1.3s, stalled 1.3s |

**Lock footprint:** Locks `orders` (blocks reads and writes).

**Findings**

- 🟠 `missing-lock-timeout` — The migration takes locks that block application queries but never sets lock_timeout. If the lock is not granted immediately (e.g. a long-running report holds a conflicting lock), the DDL waits in the lock queue and every new query on the table queues behind it: an outage that lasts as long as the longest running transaction. _Start the migration with SET lock_timeout = '2s'; so it fails fast instead of blocking everyone, and retry it._

❌ Re-tested: the suggested version still froze **checkout** for **1.5s**. It needs a human look.

<details>
<summary>Suggested safer migration</summary>

- **missing-lock-timeout** — Start with SET lock_timeout = '2s' so a DDL statement that cannot get its lock quickly fails (retry it later) instead of queueing every application query on the table behind it.

```sql
-- missing-lock-timeout
SET lock_timeout = '2s';
```

</details>

---
<sub>🎭 <b>pg-rehearse</b> ran this migration against a synthetic, production-shaped copy of your database while replaying realistic traffic. No production data was read or modified.</sub>
