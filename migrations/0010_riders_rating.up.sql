-- Riders get a rating shown to drivers on ride offers (parity with the
-- driver card riders see).
ALTER TABLE riders ADD COLUMN rating numeric(2,1) NOT NULL DEFAULT 4.8;

UPDATE riders SET rating = 4.9 WHERE id = '00000000-0000-0000-0000-000000000001';
UPDATE riders SET rating = 4.7 WHERE id = '00000000-0000-0000-0000-000000000002';
