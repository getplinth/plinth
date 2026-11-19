-- A failed request that nobody will fix can be given up by an administrator,
-- which frees the names it reserved. Anything it already created is not removed.
ALTER TABLE requests DROP CONSTRAINT requests_status_check;
ALTER TABLE requests ADD CONSTRAINT requests_status_check CHECK (status IN
  ('awaiting_approval','pending','running','succeeded','failed','rejected','cancelled','abandoned'));
