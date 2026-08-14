CREATE EXTENSION IF NOT EXISTS citext;

CREATE DOMAIN email AS text;

CREATE TABLE things (
    id      bigint PRIMARY KEY,
    small   smallint NOT NULL,
    medium  integer NOT NULL,
    big     bigint NOT NULL,
    ratio   real NOT NULL,
    precise double precision NOT NULL,
    flag    boolean NOT NULL,
    name    text NOT NULL,
    code    varchar(10) NOT NULL,
    fixed   char(2) NOT NULL,
    ci      citext NOT NULL,
    mail    email NOT NULL,
    payload bytea NOT NULL,
    doc     jsonb NOT NULL,
    legacy  json NOT NULL,
    words   text[] NOT NULL,
    counts  bigint[] NOT NULL,
    at      timestamptz NOT NULL,
    naive   timestamp NOT NULL,
    day     date NOT NULL,
    amount  numeric NOT NULL,
    ref     uuid NOT NULL,
    span    interval NOT NULL,
    -- M12.2: a range whose Go side is wrong, and one whose element has no
    -- configured mapping.
    quota   int4range NOT NULL,
    price   numrange NOT NULL,
    slots   int4multirange NOT NULL,
    -- A range column whose Go field is declared through a type alias.
    window_ tstzrange NOT NULL,
    grid    integer[][] NOT NULL
);
