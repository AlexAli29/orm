CREATE TABLE users (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE posts (
    id        bigint PRIMARY KEY,
    author_id bigint NOT NULL REFERENCES users(id),
    title     text NOT NULL
);
