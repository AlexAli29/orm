CREATE TABLE users (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);

-- Proof: a total unique constraint.
CREATE TABLE profiles (
    id      bigint PRIMARY KEY,
    user_id bigint NOT NULL UNIQUE REFERENCES users(id),
    bio     text
);

-- No proof at all.
CREATE TABLE sessions (
    id      bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id),
    token   text NOT NULL
);

-- A partial unique index constrains only the rows its WHERE clause covers, so
-- it proves nothing about the rest.
CREATE TABLE badges (
    id      bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id),
    label   text NOT NULL
);
CREATE UNIQUE INDEX badges_one_active_per_user ON badges (user_id) WHERE label = 'active';
