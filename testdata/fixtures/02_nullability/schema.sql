CREATE TABLE users (
    id       bigint PRIMARY KEY,
    nickname text,
    bio      text,
    tags     text[],
    labels   text[],
    meta     jsonb,
    email    text NOT NULL,
    legacy   text
);
