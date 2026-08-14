CREATE TABLE payments (
    id           bigint PRIMARY KEY,
    amount       numeric NOT NULL,
    ref          uuid NOT NULL,
    legacy_name  text NOT NULL,
    fee          text NOT NULL,
    fee_untagged text NOT NULL,
    -- M12.2: a range whose element is the configured numeric type, and one
    -- whose Go instantiation names the wrong one.
    band         numrange NOT NULL,
    tiers        nummultirange NOT NULL,
    wrong_band   numrange NOT NULL
);
