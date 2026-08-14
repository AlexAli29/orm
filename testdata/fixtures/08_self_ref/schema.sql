-- One self-referencing foreign key: cardinality alone decides the direction.
CREATE TABLE employees (
    id         bigint PRIMARY KEY,
    manager_id bigint REFERENCES employees(id),
    name       text NOT NULL
);

-- Two self-referencing foreign keys: the author must say which.
CREATE TABLE nodes (
    id        bigint PRIMARY KEY,
    parent_id bigint,
    origin_id bigint,
    CONSTRAINT nodes_parent_fkey FOREIGN KEY (parent_id) REFERENCES nodes(id),
    CONSTRAINT nodes_origin_fkey FOREIGN KEY (origin_id) REFERENCES nodes(id)
);
