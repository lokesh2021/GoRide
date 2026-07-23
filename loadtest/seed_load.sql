-- Provision load-test identities: 20 riders, 200 drivers. Idempotent: safe to
-- run repeatedly. Remove with loadtest/clean_load.sql.
-- mixed.js uses the first 50 drivers / 20 riders; capacity.js uses the full
-- pool so per-driver ping rate stays under the 3/s limiter during ramps.
--
--   psql -d goride -f loadtest/seed_load.sql

\set n_riders  20
\set n_drivers 200

-- Deterministic UUIDs (2000…/3000… prefixes) so load scripts can address
-- drivers by path id without a lookup: rider i = 20000000-…-<i>, driver i =
-- 30000000-…-<i>.
INSERT INTO riders (id, name, phone, api_token)
SELECT ('20000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
       'Load Rider ' || i,
       '+9190000' || lpad(i::text, 5, '0'),
       'riderload-' || i || '-token'
FROM generate_series(1, :n_riders) AS i
ON CONFLICT (api_token) DO NOTHING;

INSERT INTO drivers (id, name, phone, city, tier, vehicle_model, plate, rating, status, api_token)
SELECT ('30000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
       'Load Driver ' || i,
       '+9191000' || lpad(i::text, 5, '0'),
       'LDT',  -- dedicated load-test city shard: load supply/demand never mixes with the BLR demo pool
       (ARRAY['mini','sedan','xl'])[1 + (i % 3)],
       (ARRAY['Maruti Alto','Honda City','Toyota Innova'])[1 + (i % 3)],
       'KA-05-LT-' || lpad(i::text, 4, '0'),
       4.0 + (i % 10) / 10.0,
       'offline',
       'driverload-' || i || '-token'
FROM generate_series(1, :n_drivers) AS i
ON CONFLICT (api_token) DO NOTHING;
