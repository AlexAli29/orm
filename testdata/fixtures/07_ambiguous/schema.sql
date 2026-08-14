CREATE TABLE users (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE tags (
    id    bigint PRIMARY KEY,
    label text NOT NULL
);

-- Two foreign keys to the same table: nothing in the catalog says which one a
-- relation means.
CREATE TABLE posts (
    id        bigint PRIMARY KEY,
    author_id bigint NOT NULL,
    editor_id bigint,
    title     text NOT NULL,
    CONSTRAINT posts_author_fkey FOREIGN KEY (author_id) REFERENCES users(id),
    CONSTRAINT posts_editor_fkey FOREIGN KEY (editor_id) REFERENCES users(id)
);
