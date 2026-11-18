-- Approval step: a blueprint can require an administrator (other than the
-- requester) to approve a request before anything is provisioned.

ALTER TABLE blueprints ADD COLUMN requires_approval boolean NOT NULL DEFAULT false;

ALTER TABLE requests DROP CONSTRAINT requests_status_check;
ALTER TABLE requests ADD CONSTRAINT requests_status_check CHECK (status IN
  ('awaiting_approval','pending','running','succeeded','failed','rejected','cancelled'));

-- Who decided, when, and why (a reason is required for a rejection).
ALTER TABLE requests
  ADD COLUMN decided_by    uuid REFERENCES users(id),
  ADD COLUMN decided_at    timestamptz,
  ADD COLUMN decision_note text NOT NULL DEFAULT '';

CREATE INDEX requests_awaiting ON requests (created_at) WHERE status = 'awaiting_approval';
