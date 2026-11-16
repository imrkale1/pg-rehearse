-- Dangerous: adding a validated foreign key scans all of payments while holding
-- SHARE ROW EXCLUSIVE locks on both payments and orders (blocks their writes).
-- Safer: ADD ... NOT VALID, then VALIDATE CONSTRAINT in a separate step.
ALTER TABLE payments
    ADD CONSTRAINT payments_order_id_fkey
    FOREIGN KEY (order_id) REFERENCES orders (id);
