-- Write your migrate up statements here
CREATE TABLE delivery_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id  UUID NOT NULL REFERENCES messages(id),
    state       SMALLINT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_delivery_events_message ON delivery_events(message_id);

---- create above / drop below ----

-- Write your migrate down statements here.
DROP INDEX IF EXISTS idx_delivery_events_message;
DROP TABLE IF EXISTS delivery_events;
