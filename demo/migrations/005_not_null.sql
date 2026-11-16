-- Risky: SET NOT NULL scans every row of orders under an ACCESS EXCLUSIVE lock.
-- Safer (PG12+): ADD CONSTRAINT ... CHECK (user_id IS NOT NULL) NOT VALID;
-- VALIDATE CONSTRAINT; then SET NOT NULL (which reuses the validated check); drop the check.
ALTER TABLE orders ALTER COLUMN user_id SET NOT NULL;
