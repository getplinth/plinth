-- Notifications to send (e-mail, webhook). A row is written in the same
-- transaction as the event it reports, so a crash cannot lose it or report
-- something that did not happen; dispatchers on any replica deliver it later,
-- with retries.
CREATE TABLE notification_outbox (
  id              bigserial PRIMARY KEY,
  channel         text NOT NULL CHECK (channel IN ('email', 'webhook')),
  recipient       text NOT NULL DEFAULT '',         -- e-mail address; empty for a webhook
  event_type      text NOT NULL,
  data            jsonb NOT NULL,                   -- facts about the event, never credentials
  created_at      timestamptz NOT NULL DEFAULT now(),
  attempts        int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  claimed_at      timestamptz,                      -- lease of the dispatcher working on it
  sent_at         timestamptz,
  dead_at         timestamptz,                      -- gave up: refused for good or too many attempts
  last_error      text NOT NULL DEFAULT ''
);
CREATE INDEX notification_outbox_due ON notification_outbox (next_attempt_at)
  WHERE sent_at IS NULL AND dead_at IS NULL;
