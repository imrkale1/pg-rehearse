-- Safe: adding a nullable column without a default is a catalog-only change.
-- It still takes an ACCESS EXCLUSIVE lock, but only for a few milliseconds
-- (unless it has to queue behind a long transaction such as the admin report).
ALTER TABLE orders ADD COLUMN gift_message text;
