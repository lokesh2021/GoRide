-- Remove load-test identities and any data they produced.
--   psql -d goride -f loadtest/clean_load.sql
BEGIN;
DELETE FROM receipts r USING rides x
  WHERE r.ride_id = x.id AND (x.rider_id::text LIKE '20000000-%' OR x.driver_id::text LIKE '30000000-%');
DELETE FROM payments p USING rides x
  WHERE p.ride_id = x.id AND (x.rider_id::text LIKE '20000000-%' OR x.driver_id::text LIKE '30000000-%');
DELETE FROM trips t USING rides x
  WHERE t.ride_id = x.id AND (x.rider_id::text LIKE '20000000-%' OR x.driver_id::text LIKE '30000000-%');
DELETE FROM idempotency_keys WHERE actor_id::text LIKE '20000000-%' OR actor_id::text LIKE '30000000-%';
DELETE FROM rides WHERE rider_id::text LIKE '20000000-%' OR driver_id::text LIKE '30000000-%';
DELETE FROM quotes WHERE rider_id::text LIKE '20000000-%';
DELETE FROM drivers WHERE id::text LIKE '30000000-%';
DELETE FROM riders  WHERE id::text LIKE '20000000-%';
COMMIT;
