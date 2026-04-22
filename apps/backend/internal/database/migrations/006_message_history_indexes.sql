-- Write your migrate up statements here

CREATE INDEX IF NOT EXISTS idx_messages_dm_history
    ON messages(sender_id, recipient_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_messages_group_history
    ON messages(group_id, created_at DESC);

---- create above / drop below ----

-- Write your migrate down statements here.

DROP INDEX IF EXISTS idx_messages_group_history;
DROP INDEX IF EXISTS idx_messages_dm_history;
