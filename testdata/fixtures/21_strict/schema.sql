CREATE TABLE events (
    id       bigint PRIMARY KEY,
    name     text NOT NULL,
    happened timestamp NOT NULL,
    note     text
);
