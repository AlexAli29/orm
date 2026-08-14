CREATE TABLE users (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);

-- Deliberately not unique: many posts share an author. A belongs-to over this
-- foreign key is the most common relation there is and must not be rejected.
CREATE TABLE posts (
    id        bigint PRIMARY KEY,
    author_id bigint REFERENCES users(id),
    title     text NOT NULL
);
