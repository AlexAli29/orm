CREATE TABLE users (
    id    bigint PRIMARY KEY,
    name  text NOT NULL,
    alias text
);

CREATE TABLE items (
    id    bigint PRIMARY KEY,
    label text NOT NULL
);
