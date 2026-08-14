CREATE TABLE users (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);

CREATE VIEW active_users AS SELECT id, name FROM users;
