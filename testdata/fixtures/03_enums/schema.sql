CREATE TYPE status AS ENUM ('pending', 'active', 'banned');
CREATE TYPE color AS ENUM ('red', 'green');

CREATE TABLE items (
    id     bigint PRIMARY KEY,
    status status NOT NULL,
    color  color NOT NULL,
    shade  color NOT NULL,
    kind   text NOT NULL
);
