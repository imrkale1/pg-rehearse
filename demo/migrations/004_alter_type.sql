-- Dangerous: changing unconstrained numeric to numeric(14,2) rewrites the whole
-- orders table (and rebuilds its indexes) under an ACCESS EXCLUSIVE lock, which
-- blocks reads and writes alike.
-- Safer: add a CHECK constraint NOT VALID + VALIDATE, or new column + backfill + swap.
ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2);
