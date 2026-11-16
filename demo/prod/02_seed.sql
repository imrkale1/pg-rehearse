-- Server-side, generate_series-based seed: ~100k users, 5k products,
-- 1M orders, 2.5M order_items, ~900k payments. Deterministic (setseed).
-- Runs in well under two minutes on a laptop.

SET synchronous_commit = off;
SET maintenance_work_mem = '512MB';
SELECT setseed(0.42);

\echo 'seeding users...'
INSERT INTO users (id, email, name, country, created_at)
SELECT g,
       'user' || g || '@example.com',
       (ARRAY['Ava','Liam','Noah','Emma','Mia','Lucas','Zoe','Ethan','Aria','Mateo','Priya','Kenji'])[1 + (g % 12)]
         || ' ' ||
       (ARRAY['Smith','Garcia','Kim','Patel','Nguyen','Müller','Rossi','Silva','Cohen','Okafor'])[1 + ((g / 12) % 10)],
       (ARRAY['US','US','US','GB','DE','FR','CA','IN','BR','JP'])[1 + (g % 10)],
       now() - interval '3 years' + (g::float / 100000) * interval '3 years'
FROM generate_series(1, 100000) AS g;

\echo 'seeding products...'
INSERT INTO products (id, sku, name, category, price, stock, description, created_at)
SELECT g,
       'SKU-' || lpad(g::text, 6, '0'),
       initcap((ARRAY['classic','premium','eco','ultra','mini','pro'])[1 + (g % 6)]) || ' '
         || (ARRAY['mug','hoodie','lamp','backpack','notebook','headphones','sneakers','kettle'])[1 + (g % 8)]
         || ' #' || g,
       (ARRAY['kitchen','apparel','home','bags','stationery','audio','shoes','garden'])[1 + (g % 8)],
       round((5 + random() * 295)::numeric, 2),
       (random() * 500)::int,
       'A very nice product. ' || repeat('Lorem ipsum dolor sit amet. ', 1 + (g % 5)),
       now() - interval '3 years' + random() * interval '3 years'
FROM generate_series(1, 5000) AS g;

\echo 'seeding orders...'
-- Skewed: a minority of users place most orders; recent months are busier.
INSERT INTO orders (id, user_id, status, total, currency, created_at, updated_at)
SELECT g,
       1 + floor(power(random(), 2) * 100000)::bigint,
       CASE WHEN r < 0.02 THEN 'pending'
            WHEN r < 0.08 THEN 'paid'
            WHEN r < 0.18 THEN 'shipped'
            WHEN r < 0.92 THEN 'delivered'
            WHEN r < 0.97 THEN 'cancelled'
            ELSE 'refunded' END::order_status,
       0,
       'USD',
       ts,
       ts + interval '2 days' * random()
FROM (
    SELECT g, random() AS r,
           now() - interval '3 years' * (1 - sqrt(g::float / 1000000)) AS ts
    FROM generate_series(1, 1000000) AS g
) s;

\echo 'seeding order_items...'
INSERT INTO order_items (order_id, product_id, quantity, unit_price)
SELECT 1 + (g % 1000000),
       1 + floor(power(random(), 1.5) * 5000)::bigint,
       1 + floor(power(random(), 3) * 4)::int,
       round((5 + random() * 295)::numeric, 2)
FROM generate_series(0, 2499999) AS g;

\echo 'computing order totals...'
UPDATE orders o
SET total = t.total
FROM (SELECT order_id, sum(quantity * unit_price) AS total
      FROM order_items GROUP BY order_id) t
WHERE o.id = t.order_id;

\echo 'seeding payments...'
INSERT INTO payments (order_id, provider, provider_ref, amount, status, created_at)
SELECT o.id,
       (ARRAY['stripe','stripe','stripe','adyen','paypal'])[1 + (o.id % 5)::int],
       'pay_' || md5(o.id::text),
       o.total,
       CASE o.status WHEN 'refunded' THEN 'refunded'
                     WHEN 'cancelled' THEN 'failed'
                     WHEN 'paid' THEN 'authorized'
                     ELSE 'captured' END::payment_status,
       o.created_at + interval '1 minute'
FROM orders o
WHERE o.status <> 'pending';

-- Keep identity sequences ahead of the explicit ids used above.
SELECT setval(pg_get_serial_sequence('users', 'id'),    (SELECT max(id) FROM users));
SELECT setval(pg_get_serial_sequence('products', 'id'), (SELECT max(id) FROM products));
SELECT setval(pg_get_serial_sequence('orders', 'id'),   (SELECT max(id) FROM orders));

\echo 'vacuum analyze...'
VACUUM (ANALYZE) users, products, orders, order_items, payments;
SELECT pg_stat_statements_reset();
\echo 'seed complete'
