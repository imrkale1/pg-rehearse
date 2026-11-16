-- Dangerous: a plain CREATE INDEX holds a SHARE lock on orders for the whole
-- build (~0.5s on an idle 1M-row copy, longer under real I/O pressure and with
-- production-sized tables), blocking every INSERT/UPDATE — i.e. checkout.
-- Safer: CREATE INDEX CONCURRENTLY (outside a transaction).
CREATE INDEX orders_created_at_idx ON orders (created_at);
