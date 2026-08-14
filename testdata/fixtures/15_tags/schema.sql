CREATE TABLE users (
    id           bigint PRIMARY KEY,
    name         text NOT NULL,
    unknown      text NOT NULL,
    fk_on_scalar text NOT NULL
);

CREATE TABLE posts (
    id        bigint PRIMARY KEY,
    author_id bigint NOT NULL UNIQUE REFERENCES users(id),
    title     text NOT NULL
);
