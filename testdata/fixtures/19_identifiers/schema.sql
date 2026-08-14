CREATE TABLE billing_addresses (
    id   bigint PRIMARY KEY,
    line text NOT NULL
);

CREATE TABLE shipping_addresses (
    id   bigint PRIMARY KEY,
    line text NOT NULL
);
