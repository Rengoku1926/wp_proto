-- Write your migrate up statements here
CREATE TABLE groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id UUID NOT NULL REFERENCES groups(id),
    user_id UUID NOT NULL REFERENCES users(id),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);

CREATE INDEX idx_group_members_group ON group_members(group_id);
CREATE INDEX idx_group_members_user ON group_members(user_id);

---- create above / drop below ----

-- Write your migrate down statements here. If this migration is irreversible
DROP INDEX IF EXISTS idx_group_members_user;
DROP INDEX IF EXISTS idx_group_members_group;
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS groups;
-- Then delete the separator line above.
