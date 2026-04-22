-- Write your migrate up statements here
CREATE TABLE messages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id UUID UNIQUE NOT NULL, 
    sender_id UUID NOT NULL REFERENCES users(id),
    recipient_id UUID REFERENCES users(id),
    group_id UUID, 
    body TEXT NOT NULL, 
    state SMALLINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_messages_sender ON messages(sender_id);
CREATE INDEX idx_messages_recipient ON messages(recipient_id);
CREATE INDEX idx_messages_client_id ON messages(client_id);

---- create above / drop below ----

-- Write your migrate down statements here. If this migration is irreversible
DROP TABLE IF EXISTS messages
-- Then delete the separator line above.
