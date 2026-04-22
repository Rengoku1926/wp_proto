-- Write your migrate up statements here
CREATE TABLE group_message_delivery (
    message_id UUID NOT NULL REFERENCES messages(id),
    member_id UUID NOT NULL REFERENCES users(id),
    state SMALLINT NOT NULL DEFAULT 1,
    PRIMARY KEY (message_id, member_id)
)

---- create above / drop below ----

-- Write your migrate down statements here. If this migration is irreversible
DROP TABLE IF EXISTS group_message_delivery;
-- Then delete the separator line above.
