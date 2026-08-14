-- The spatial demo schema.
--
-- It is deliberately a schema somebody would really write: places with a
-- geography point so that distances are in metres, delivery zones with a
-- geometry multi-polygon so that containment is exact, roads as lines, and a
-- table of readings with the dimensionalities PostGIS distinguishes.
--
-- The confusable columns are on purpose. places has a geometry(Point,4326), a
-- geometry(Point,3857) and a geography(Point,4326) side by side, because three
-- columns that all hold a point and mean different things is exactly the shape
-- a positional bug hides in.

CREATE EXTENSION IF NOT EXISTS postgis;

CREATE TABLE places (
    id         bigint PRIMARY KEY,
    name       text NOT NULL,
    -- Where it is, on the spheroid: distances from this are in metres.
    spot       geography(Point, 4326) NOT NULL,
    -- The same place in the plane, for the operations geography does not have.
    location   geometry(Point, 4326) NOT NULL,
    -- And in web Mercator, which is a different coordinate system holding
    -- numbers that look just as plausible.
    projected  geometry(Point, 3857),
    -- A footprint the place may not have.
    footprint  geometry(Polygon, 4326),
    -- Any geometry at all, which is what a column with no modifier accepts.
    sketch     geometry,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX places_spot_gist ON places USING gist (spot);
CREATE INDEX places_location_gist ON places USING gist (location);

CREATE TABLE zones (
    id      bigint PRIMARY KEY,
    name    text NOT NULL,
    area    geometry(MultiPolygon, 4326) NOT NULL,
    centre  geometry(Point, 4326)
);

CREATE INDEX zones_area_gist ON zones USING gist (area);

CREATE TABLE roads (
    id    bigint PRIMARY KEY,
    name  text NOT NULL,
    path  geometry(LineString, 4326) NOT NULL
);

CREATE INDEX roads_path_gist ON roads USING gist (path);

-- The dimensionalities. PostGIS keeps four and this table has all four, so that
-- introspection, the generated descriptor, COPY and the codec are each tested
-- against a column that really carries a Z, an M, or both.
CREATE TABLE readings (
    id     bigint PRIMARY KEY,
    flat   geometry(Point, 4326) NOT NULL,
    raised geometry(PointZ, 4326) NOT NULL,
    marked geometry(PointM, 4326) NOT NULL,
    zm     geometry(PointZM, 4326) NOT NULL,
    line3d geometry(LineStringZ, 4326)
);
