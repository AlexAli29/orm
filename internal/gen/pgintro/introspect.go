package pgintro

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/jackc/pgx/v5"
)

// relkinds are the relation kinds v1 accepts as entity tables: ordinary and
// partitioned. Everything else — views, materialized views, foreign tables,
// sequences — is not a table an entity can be written through.
var relkinds = []string{"r", "p"}

// viewRelkinds are the relation kinds that are a stored query: an ordinary view
// and a materialized view. PostgreSQL's own catalog is the authority for which
// is which — nothing here reads a name and guesses.
var viewRelkinds = []string{"v", "m"}

// readableRelkinds is everything a SELECT can read, which is what column
// introspection covers.
var readableRelkinds = []string{"r", "p", "v", "m"}

// uniqueIndexRelkinds are the relations that can carry an index. An ordinary
// view cannot; a materialized view can, and the unique one it carries is what
// REFRESH CONCURRENTLY requires — so leaving it out would have made every
// concurrent refresh look impossible.
var uniqueIndexRelkinds = []string{"r", "p", "m"}

// notExtensionOwned excludes relations that belong to an installed extension.
//
// PostGIS installs geometry_columns and geography_columns as ordinary views in
// the search path, and an extension's objects are not the project's schema: the
// project did not declare them, must not generate query sources for them, and
// must never plan a migration that touches them. Before views were introspected
// this did not arise, because extensions rarely install plain tables into a
// user schema — the moment views were included, PostGIS's own catalog views
// arrived with them.
//
// pg_depend records extension membership with deptype 'e', which is the
// catalog's own answer to "does this belong to an extension" and is the only
// one worth asking.
const notExtensionOwned = `
  AND NOT EXISTS (
      SELECT 1 FROM pg_depend ed
      WHERE ed.objid = c.oid
        AND ed.classid = 'pg_class'::regclass
        AND ed.refclassid = 'pg_extension'::regclass
        AND ed.deptype = 'e'
  )`

// Connect opens a connection using dsn.
func Connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	return conn, nil
}

// Introspect reads every table in searchPath and returns the resolved schema.
// The schemas are read in the order given, which is also the order an
// unqualified table reference resolves in.
func Introspect(ctx context.Context, conn *pgx.Conn, searchPath []string) (*model.Schema, error) {
	if len(searchPath) == 0 {
		return nil, fmt.Errorf("introspection needs a non-empty search path")
	}

	types, err := loadTypes(ctx, conn, searchPath)
	if err != nil {
		return nil, err
	}
	tables, byOID, err := loadTables(ctx, conn, searchPath)
	if err != nil {
		return nil, err
	}
	views, viewsByOID, err := loadRelations(ctx, conn, searchPath, viewRelkinds)
	if err != nil {
		return nil, err
	}
	// Columns are loaded for every readable relation at once, through the same
	// query and the same type resolution. A view's column is a column: the same
	// PostgreSQL type, the same modifier, the same array dimensions. A second
	// path for view columns would be a second place for a type to be resolved
	// differently, which is how a view ends up with a column typed text because
	// nobody taught the copy about domains.
	all := make(map[uint32]*model.PGTable, len(byOID)+len(viewsByOID))
	for oid, t := range byOID {
		all[oid] = t
	}
	for oid, v := range viewsByOID {
		all[oid] = v
	}
	colsByOID, err := loadColumns(ctx, conn, searchPath, all, types)
	if err != nil {
		return nil, err
	}
	if len(views) > 0 {
		if err := loadViewDetail(ctx, conn, searchPath, viewsByOID); err != nil {
			return nil, err
		}
	}
	if err := loadConstraints(ctx, conn, searchPath, byOID, colsByOID); err != nil {
		return nil, err
	}
	if err := loadUniqueIndexes(ctx, conn, searchPath, all, colsByOID); err != nil {
		return nil, err
	}

	byQualified := func(a, b *model.PGTable) int {
		return cmp.Or(cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Name, b.Name))
	}
	slices.SortFunc(tables, byQualified)
	slices.SortFunc(views, byQualified)
	return model.NewSchemaWithViews(slices.Clone(searchPath), tables, views), nil
}

const typesQuery = `
WITH RECURSIVE used AS (
    SELECT DISTINCT a.atttypid AS oid
    FROM pg_attribute a
    JOIN pg_class c ON c.oid = a.attrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = ANY($1)
      AND c.relkind = ANY($2)` + notExtensionOwned + `
      AND a.attnum > 0
      AND NOT a.attisdropped
), closure AS (
    SELECT oid FROM used
  UNION
    SELECT ref.oid
    FROM closure cl
    JOIN pg_type t ON t.oid = cl.oid
    LEFT JOIN pg_range rng ON rng.rngtypid = t.oid
    LEFT JOIN pg_range mrng ON mrng.rngmultitypid = t.oid
    CROSS JOIN LATERAL (VALUES
        (t.typelem), (t.typbasetype),
        (coalesce(rng.rngsubtype, 0)), (coalesce(mrng.rngtypid, 0))
    ) AS ref(oid)
    WHERE ref.oid <> 0
)
SELECT t.oid, n.nspname, t.typname, t.typtype::text, t.typcategory::text,
       t.typelem, t.typbasetype, t.typnotnull,
       coalesce(rng.rngsubtype, mrng.rngtypid, 0) AS rngelem
FROM closure cl
JOIN pg_type t ON t.oid = cl.oid
JOIN pg_namespace n ON n.oid = t.typnamespace
LEFT JOIN pg_range rng ON rng.rngtypid = t.oid
LEFT JOIN pg_range mrng ON mrng.rngmultitypid = t.oid
ORDER BY t.oid`

const enumQuery = `
SELECT e.enumtypid, e.enumlabel
FROM pg_enum e
ORDER BY e.enumtypid, e.enumsortorder`

// loadTypes resolves every type reachable from a column of an introspected
// relation, following array element and domain base types to closure.
//
// The scope is readableRelkinds, not relkinds, because that is the scope
// loadColumns resolves against: a view's column is resolved through this same
// map, and a stored query can produce a type no table column has. SELECT
// count(*) yields bigint whether or not any table in the schema stores one, and
// discovering types from tables alone left that column pointing at an OID the
// map had never heard of.
func loadTypes(ctx context.Context, conn *pgx.Conn, searchPath []string) (map[uint32]*model.PGType, error) {
	rows, err := conn.Query(ctx, typesQuery, searchPath, readableRelkinds)
	if err != nil {
		return nil, fmt.Errorf("querying pg_type: %w", err)
	}
	// A range's subtype and a multirange's range type live in pg_range rather
	// than in typelem, so they arrive as a third link and are followed the same
	// way. That is what makes int4multirange -> int4range -> int4 a chain the
	// reconciler can walk instead of a name it has to recognise.
	type link struct{ elem, base, rng uint32 }
	links := make(map[uint32]link)
	types := make(map[uint32]*model.PGType)

	for rows.Next() {
		var (
			oid             uint32
			schema, name    string
			typtype, typcat string
			elem, base, rng uint32
			notNull         bool
		)
		if err := rows.Scan(&oid, &schema, &name, &typtype, &typcat, &elem, &base, &notNull, &rng); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning pg_type: %w", err)
		}
		types[oid] = &model.PGType{
			OID:           oid,
			Schema:        schema,
			Name:          name,
			Kind:          typeKind(typtype, typcat, elem),
			DomainNotNull: notNull,
		}
		links[oid] = link{elem: elem, base: base, rng: rng}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading pg_type: %w", err)
	}

	for oid, t := range types {
		l := links[oid]
		switch t.Kind {
		case model.PGArray:
			t.Elem = types[l.elem]
		case model.PGDomain:
			t.Elem = types[l.base]
		case model.PGRange:
			// The subtype: int4range's element is int4.
			t.Elem = types[l.rng]
		case model.PGMultirange:
			// The range type, not the element: int4multirange's element is
			// int4range, whose own element is int4. Keeping the chain intact is
			// what lets a multirange be described without a table of names.
			t.Elem = types[l.rng]
		}
	}

	labelRows, err := conn.Query(ctx, enumQuery)
	if err != nil {
		return nil, fmt.Errorf("querying pg_enum: %w", err)
	}
	defer labelRows.Close()
	for labelRows.Next() {
		var (
			oid   uint32
			label string
		)
		if err := labelRows.Scan(&oid, &label); err != nil {
			return nil, fmt.Errorf("scanning pg_enum: %w", err)
		}
		if t, ok := types[oid]; ok {
			t.Labels = append(t.Labels, label)
		}
	}
	if err := labelRows.Err(); err != nil {
		return nil, fmt.Errorf("reading pg_enum: %w", err)
	}
	return types, nil
}

// typeKind maps pg_type.typtype onto the model's kinds, promoting base types
// with an element type and array category to PGArray.
func typeKind(typtype, category string, elem uint32) model.PGTypeKind {
	switch typtype {
	case "e":
		return model.PGEnum
	case "d":
		return model.PGDomain
	case "c":
		return model.PGComposite
	case "r":
		return model.PGRange
	case "m":
		return model.PGMultirange
	case "p":
		return model.PGPseudo
	default:
		if category == "A" && elem != 0 {
			return model.PGArray
		}
		return model.PGBase
	}
}

const tablesQuery = `
SELECT c.oid, n.nspname, c.relname, c.relkind::text
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind = ANY($2)` + notExtensionOwned + `
ORDER BY n.nspname, c.relname`

func loadTables(ctx context.Context, conn *pgx.Conn, searchPath []string) ([]*model.PGTable, map[uint32]*model.PGTable, error) {
	return loadRelations(ctx, conn, searchPath, relkinds)
}

// loadRelations reads pg_class for the given relation kinds.
func loadRelations(ctx context.Context, conn *pgx.Conn, searchPath []string, kinds []string) ([]*model.PGTable, map[uint32]*model.PGTable, error) {
	rows, err := conn.Query(ctx, tablesQuery, searchPath, kinds)
	if err != nil {
		return nil, nil, fmt.Errorf("querying pg_class: %w", err)
	}
	defer rows.Close()

	var tables []*model.PGTable
	byOID := make(map[uint32]*model.PGTable)
	for rows.Next() {
		var (
			oid          uint32
			schema, name string
			kind         string
		)
		if err := rows.Scan(&oid, &schema, &name, &kind); err != nil {
			return nil, nil, fmt.Errorf("scanning pg_class: %w", err)
		}
		t := &model.PGTable{OID: oid, Schema: schema, Name: name, Kind: kind[0]}
		tables = append(tables, t)
		byOID[oid] = t
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading pg_class: %w", err)
	}
	return tables, byOID, nil
}

const columnsQuery = `
SELECT a.attrelid, a.attname, a.attnum, a.atttypid, a.attnotnull, a.atthasdef,
       a.attidentity::text, a.attgenerated::text, a.attndims,
       format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind = ANY($2)` + notExtensionOwned + `
  AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attrelid, a.attnum`

// colKey identifies a column by table OID and attribute number, which is how
// every other catalog refers to one.
type colKey struct {
	rel    uint32
	attnum int
}

func loadColumns(ctx context.Context, conn *pgx.Conn, searchPath []string, byOID map[uint32]*model.PGTable, types map[uint32]*model.PGType) (map[colKey]*model.PGColumn, error) {
	rows, err := conn.Query(ctx, columnsQuery, searchPath, readableRelkinds)
	if err != nil {
		return nil, fmt.Errorf("querying pg_attribute: %w", err)
	}
	defer rows.Close()

	cols := make(map[colKey]*model.PGColumn)
	for rows.Next() {
		var (
			rel                 uint32
			name                string
			attnum              int16
			typeOID             uint32
			notNull, hasDefault bool
			identity, generated string
			dims                int32
			formatted           string
		)
		if err := rows.Scan(&rel, &name, &attnum, &typeOID, &notNull, &hasDefault,
			&identity, &generated, &dims, &formatted); err != nil {
			return nil, fmt.Errorf("scanning pg_attribute: %w", err)
		}
		table, ok := byOID[rel]
		if !ok {
			continue
		}
		c := &model.PGColumn{
			Table:      table,
			Name:       name,
			AttNum:     int(attnum),
			Type:       types[typeOID],
			NotNull:    notNull,
			HasDefault: hasDefault,
			Identity:   firstByte(identity),
			Generated:  firstByte(generated),
			Dims:       int(dims),
			Formatted:  formatted,
		}
		if c.Type == nil {
			return nil, fmt.Errorf("column %s.%s.%s has type OID %d, which was not loaded", table.Schema, table.Name, name, typeOID)
		}
		// A stored generated column reports atthasdef, but the two facts mean
		// different things to a writer, so keep them apart.
		if c.IsGenerated() {
			c.HasDefault = false
		}
		table.Cols = append(table.Cols, c)
		cols[colKey{rel: rel, attnum: int(attnum)}] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading pg_attribute: %w", err)
	}
	return cols, nil
}

func firstByte(s string) byte {
	if s == "" {
		return 0
	}
	return s[0]
}

const constraintsQuery = `
SELECT con.conname, con.contype::text, con.conrelid, con.confrelid, con.conkey, con.confkey
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind = ANY($2) AND con.contype IN ('p', 'f')
ORDER BY con.conname, con.oid`

// loadConstraints reads primary and foreign keys. Column order comes straight
// from conkey and confkey, whose ordinality is the pairing between the two
// sides; comparing them as sets would silently pair the wrong columns.
func loadConstraints(ctx context.Context, conn *pgx.Conn, searchPath []string, byOID map[uint32]*model.PGTable, cols map[colKey]*model.PGColumn) error {
	rows, err := conn.Query(ctx, constraintsQuery, searchPath, relkinds)
	if err != nil {
		return fmt.Errorf("querying pg_constraint: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			name            string
			contype         string
			relOID, refOID  uint32
			conkey, confkey []int16
		)
		if err := rows.Scan(&name, &contype, &relOID, &refOID, &conkey, &confkey); err != nil {
			return fmt.Errorf("scanning pg_constraint: %w", err)
		}
		table, ok := byOID[relOID]
		if !ok {
			continue
		}
		local, err := resolveCols(cols, relOID, conkey)
		if err != nil {
			return fmt.Errorf("constraint %s: %w", name, err)
		}
		switch contype {
		case "p":
			table.PK = local
			table.PKName = name
		case "f":
			// A foreign key whose referenced table lies outside the search
			// path cannot back a relation between two mapped entities, so it
			// is dropped rather than half-resolved.
			refTable, ok := byOID[refOID]
			if !ok {
				continue
			}
			remote, err := resolveCols(cols, refOID, confkey)
			if err != nil {
				return fmt.Errorf("constraint %s: %w", name, err)
			}
			if len(local) != len(remote) {
				return fmt.Errorf("constraint %s: %d referencing columns against %d referenced columns", name, len(local), len(remote))
			}
			table.FKs = append(table.FKs, &model.PGForeignKey{
				Name:     name,
				Table:    table,
				RefTable: refTable,
				Cols:     local,
				RefCols:  remote,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading pg_constraint: %w", err)
	}
	return nil
}

func resolveCols(cols map[colKey]*model.PGColumn, rel uint32, attnums []int16) ([]*model.PGColumn, error) {
	out := make([]*model.PGColumn, 0, len(attnums))
	for _, n := range attnums {
		c, ok := cols[colKey{rel: rel, attnum: int(n)}]
		if !ok {
			return nil, fmt.Errorf("references attribute %d of relation %d, which was not loaded", n, rel)
		}
		out = append(out, c)
	}
	return out, nil
}

const uniqueQuery = `
SELECT ic.relname, i.indrelid, i.indnkeyatts,
       (SELECT array_agg(k ORDER BY ord) FROM unnest(i.indkey::int2[]) WITH ORDINALITY AS u(k, ord)),
       i.indpred IS NOT NULL, i.indexprs IS NOT NULL, i.indisprimary
FROM pg_index i
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_class ic ON ic.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind = ANY($2)
  AND i.indisunique AND i.indisvalid AND i.indisready
ORDER BY ic.relname, i.indexrelid`

// loadUniqueIndexes reads every valid unique index, including the ones that do
// not prove anything. A partial or expression index is recorded with its flag
// set rather than dropped, so that reconciliation can say why an index the
// author clearly wrote was not accepted as proof of uniqueness.
func loadUniqueIndexes(ctx context.Context, conn *pgx.Conn, searchPath []string, byOID map[uint32]*model.PGTable, cols map[colKey]*model.PGColumn) error {
	rows, err := conn.Query(ctx, uniqueQuery, searchPath, uniqueIndexRelkinds)
	if err != nil {
		return fmt.Errorf("querying pg_index: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			name              string
			relOID            uint32
			keyAtts           int16
			indkey            []int16
			partial, hasExprs bool
			primary           bool
		)
		if err := rows.Scan(&name, &relOID, &keyAtts, &indkey, &partial, &hasExprs, &primary); err != nil {
			return fmt.Errorf("scanning pg_index: %w", err)
		}
		table, ok := byOID[relOID]
		if !ok {
			continue
		}
		// Trailing INCLUDE columns are stored but not part of the key, so they
		// contribute nothing to uniqueness.
		if int(keyAtts) < len(indkey) {
			indkey = indkey[:keyAtts]
		}
		u := model.PGUnique{Name: name, Partial: partial, Expression: hasExprs, Primary: primary}
		for _, n := range indkey {
			if n == 0 {
				// Attribute number zero marks an expression key; the index
				// constrains something other than a plain column.
				u.Expression = true
				continue
			}
			c, ok := cols[colKey{rel: relOID, attnum: int(n)}]
			if !ok {
				return fmt.Errorf("index %s references attribute %d, which was not loaded", name, n)
			}
			u.Cols = append(u.Cols, c)
		}
		table.Uniques = append(table.Uniques, u)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading pg_index: %w", err)
	}
	return nil
}
