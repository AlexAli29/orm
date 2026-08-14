CREATE TABLE users (
    id        bigint NOT NULL,
    tenant_id bigint NOT NULL,
    name      text NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (tenant_id, id)
);

-- The two sides of the composite foreign key are in different relative column
-- order on their tables: posts(tenant_id, author_id) against
-- users(tenant_id, id). Comparing column sets instead of conkey and confkey
-- ordinality would pair author_id with tenant_id and still look plausible.
CREATE TABLE posts (
    tenant_id bigint NOT NULL,
    id        bigint NOT NULL,
    author_id bigint NOT NULL,
    title     text NOT NULL,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT posts_author_fkey FOREIGN KEY (tenant_id, author_id) REFERENCES users(tenant_id, id)
);
