package pgintro

import (
	"context"
	"fmt"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/jackc/pgx/v5"
)

// Reading a live database into the canonical schema model.
//
// This is the half of the correctness loop that closes it: a desired schema
// becomes migrations, the migrations run, and what PostgreSQL ended up with is
// read back and compared against what was asked for. Without it, the engine
// could only prove that its own operations agree with its own state.
//
// It is an additional pass over the same catalog in the same package rather
// than a second implementation elsewhere. It reads more than [Introspect] does
// — defaults, checks, every index rather than only the unique ones, referential
// actions — because reconciliation and migration ask the catalog different
// questions: reconciliation asks whether a Go field can hold a column, and this
// asks what would have to change for the schema to become something else.
//
// Everything PostgreSQL keeps for its own purposes is dropped on the way: OIDs,
// creation order, the names of things nobody named. Semantic equality must not
// depend on any of it.

// Canonical reads searchPath into the canonical schema model.
func Canonical(ctx context.Context, conn *pgx.Conn, searchPath []string) (*schema.Schema, error) {
	if len(searchPath) == 0 {
		return nil, fmt.Errorf("introspection needs a non-empty search path")
	}
	s := &schema.Schema{}

	enums, err := canonicalEnums(ctx, conn, searchPath)
	if err != nil {
		return nil, err
	}
	s.Enums = enums

	tables, err := canonicalTables(ctx, conn, searchPath)
	if err != nil {
		return nil, err
	}
	s.Tables = tables

	viewList, matList, err := canonicalViews(ctx, conn, searchPath)
	if err != nil {
		return nil, err
	}
	s.Views, s.MaterializedViews = viewList, matList

	extensions, err := canonicalExtensions(ctx, conn)
	if err != nil {
		return nil, err
	}
	s.Extensions = extensions

	s.Normalize()
	return s, nil
}

// canonicalExtensions reads the extensions installed in the database.
//
// plpgsql is excluded: PostgreSQL creates it in every database from the
// template, so it is present whether anybody asked for it or not, and reporting
// it would put an object in every inspected schema that no declaration will
// ever match.
//
// Extensions are database-wide rather than schema-scoped — the search path says
// where their objects live, not whether they exist — so this is not filtered by
// it. Nothing drops an extension, so an installed one nobody declared is
// reported and then ignored by the diff.
func canonicalExtensions(ctx context.Context, conn *pgx.Conn) ([]schema.Extension, error) {
	rows, err := conn.Query(ctx, `
SELECT e.extname, n.nspname
FROM pg_extension e
JOIN pg_namespace n ON n.oid = e.extnamespace
WHERE e.extname <> 'plpgsql'
ORDER BY e.extname`)
	if err != nil {
		return nil, fmt.Errorf("reading extensions: %w", err)
	}
	defer rows.Close()

	var out []schema.Extension
	for rows.Next() {
		var name, namespace string
		if err := rows.Scan(&name, &namespace); err != nil {
			return nil, fmt.Errorf("reading extensions: %w", err)
		}
		// The schema is recorded only when it is not the default one, so an
		// extension installed the ordinary way compares equal to a declaration
		// that did not name a schema.
		if namespace == "public" {
			namespace = ""
		}
		out = append(out, schema.Extension{Name: name, Schema: namespace})
	}
	return out, rows.Err()
}

const canonicalEnumsQuery = `
SELECT n.nspname, t.typname,
       array_agg(e.enumlabel::text ORDER BY e.enumsortorder)
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
JOIN pg_enum e ON e.enumtypid = t.oid
WHERE n.nspname = ANY($1)
  AND NOT EXISTS (SELECT 1 FROM pg_depend dep
                  WHERE dep.classid = 'pg_type'::regclass
                    AND dep.objid = t.oid AND dep.deptype = 'e')
GROUP BY n.nspname, t.typname
ORDER BY n.nspname, t.typname`

func canonicalEnums(ctx context.Context, conn *pgx.Conn, searchPath []string) ([]schema.Enum, error) {
	rows, err := conn.Query(ctx, canonicalEnumsQuery, searchPath)
	if err != nil {
		return nil, fmt.Errorf("reading enum types: %w", err)
	}
	defer rows.Close()

	var out []schema.Enum
	for rows.Next() {
		var e schema.Enum
		if err := rows.Scan(&e.Schema, &e.Name, &e.Labels); err != nil {
			return nil, fmt.Errorf("reading enum types: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// canonicalColumnsQuery reads every column of every table, with the properties
// a migration has to be able to change.
//
// format_type gives the type as PostgreSQL itself would write it, which is what
// a DDL statement needs and what makes int8 and public.user_state come back
// distinguishable without resolving anything by hand.
const canonicalColumnsQuery = `
SELECT n.nspname, c.relname, a.attname, a.attnum,
       format_type(a.atttypid, a.atttypmod),
       tn.nspname, t.typname, t.typcategory::text,
       COALESCE(en.nspname, ''), COALESCE(et.typname, ''),
       NOT a.attnotnull,
       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
       a.attidentity::text,
       a.attgenerated::text,
       COALESCE(co.collname, ''),
       EXISTS (SELECT 1 FROM pg_depend dep
               WHERE dep.classid = 'pg_type'::regclass
                 AND dep.objid = t.oid AND dep.deptype = 'e')
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = a.atttypid
JOIN pg_namespace tn ON tn.oid = t.typnamespace
LEFT JOIN pg_type et ON et.oid = t.typelem AND t.typcategory = 'A'
LEFT JOIN pg_namespace en ON en.oid = et.typnamespace
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
LEFT JOIN pg_collation co ON co.oid = a.attcollation AND co.collname <> 'default'
WHERE n.nspname = ANY($1) AND c.relkind = ANY($2)
  AND a.attnum > 0 AND NOT a.attisdropped
  AND NOT EXISTS (SELECT 1 FROM pg_depend dep
                  WHERE dep.classid = 'pg_class'::regclass
                    AND dep.objid = c.oid AND dep.deptype = 'e')
ORDER BY n.nspname, c.relname, a.attnum`

// canonicalConstraintsQuery reads primary keys, uniques, foreign keys and
// checks in one pass, with everything about them that has to survive a
// round-trip.
const canonicalConstraintsQuery = `
SELECT n.nspname, c.relname, con.conname, con.contype::text,
       COALESCE((SELECT array_agg(a.attname ORDER BY u.ord)
                 FROM unnest(con.conkey) WITH ORDINALITY AS u(attnum, ord)
                 JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = u.attnum), '{}'),
       COALESCE(rn.nspname, ''), COALESCE(rc.relname, ''),
       COALESCE((SELECT array_agg(a.attname ORDER BY u.ord)
                 FROM unnest(con.confkey) WITH ORDINALITY AS u(attnum, ord)
                 JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = u.attnum), '{}'),
       con.confdeltype::text, con.confupdtype::text,
       con.condeferrable, con.condeferred, con.convalidated,
       COALESCE(pg_get_constraintdef(con.oid), '')
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_class rc ON rc.oid = con.confrelid
LEFT JOIN pg_namespace rn ON rn.oid = rc.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind = ANY($2)
  AND con.contype IN ('p', 'u', 'f', 'c')
  AND NOT EXISTS (SELECT 1 FROM pg_depend dep
                  WHERE dep.classid = 'pg_class'::regclass
                    AND dep.objid = c.oid AND dep.deptype = 'e')
ORDER BY n.nspname, c.relname, con.conname`

// canonicalIndexesQuery reads every index, including the non-unique ones the
// reconciler has no use for.
//
// The definition comes back as PostgreSQL renders it, which is the only source
// for an expression key, an operator class or a predicate that survives being
// re-rendered. The structured columns beside it carry the parts that can be
// read directly.
const canonicalIndexesQuery = `
SELECT n.nspname, c.relname, ic.relname,
       i.indisunique, i.indnkeyatts, am.amname,
       pg_get_indexdef(i.indexrelid),
       COALESCE(pg_get_expr(i.indpred, i.indrelid), ''),
       con.contype IS NOT NULL
FROM pg_index i
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_class ic ON ic.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_am am ON am.oid = ic.relam
LEFT JOIN pg_constraint con ON con.conindid = i.indexrelid AND con.contype IN ('p', 'u')
WHERE n.nspname = ANY($1) AND c.relkind = ANY($2)
  AND i.indisvalid AND i.indisready
  AND NOT EXISTS (SELECT 1 FROM pg_depend dep
                  WHERE dep.classid = 'pg_class'::regclass
                    AND dep.objid = c.oid AND dep.deptype = 'e')
ORDER BY n.nspname, c.relname, ic.relname`

func canonicalTables(ctx context.Context, conn *pgx.Conn, searchPath []string) ([]schema.Table, error) {
	byName := make(map[string]*schema.Table)
	order := make([]string, 0, 16)
	get := func(schemaName, name string) *schema.Table {
		key := schemaName + "." + name
		if t, ok := byName[key]; ok {
			return t
		}
		t := &schema.Table{Schema: schemaName, Name: name}
		byName[key] = t
		order = append(order, key)
		return t
	}

	if err := readColumns(ctx, conn, searchPath, get); err != nil {
		return nil, err
	}
	if err := readConstraints(ctx, conn, searchPath, byName); err != nil {
		return nil, err
	}
	if err := readIndexes(ctx, conn, searchPath, byName); err != nil {
		return nil, err
	}

	out := make([]schema.Table, 0, len(order))
	for _, key := range order {
		out = append(out, *byName[key])
	}
	return out, nil
}

func readColumns(ctx context.Context, conn *pgx.Conn, searchPath []string, get func(string, string) *schema.Table) error {
	rows, err := conn.Query(ctx, canonicalColumnsQuery, searchPath, relkinds)
	if err != nil {
		return fmt.Errorf("reading columns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			schemaName, table, name        string
			attnum                         int16
			rendered                       string
			typeSchema, typeName, category string
			elemSchema, elemName           string
			nullable                       bool
			def                            string
			identity, generated            string
			collation                      string
			fromExtension                  bool
		)
		if err := rows.Scan(&schemaName, &table, &name, &attnum, &rendered,
			&typeSchema, &typeName, &category, &elemSchema, &elemName,
			&nullable, &def, &identity, &generated, &collation, &fromExtension); err != nil {
			return fmt.Errorf("reading columns: %w", err)
		}

		c := schema.Column{
			Name:      name,
			Type:      columnType(rendered, typeSchema, typeName, category, elemSchema, elemName, fromExtension),
			Nullable:  nullable,
			Collation: collation,
		}
		switch firstByte(identity) {
		case 'a':
			c.Identity = schema.IdentityAlways
		case 'd':
			c.Identity = schema.IdentityByDefault
		}
		// A generated column reports a default too, because its expression is
		// stored there. The two mean different things and only one of them is a
		// default.
		if firstByte(generated) == 's' {
			c.Generated = schema.Expr(def)
		} else if c.Identity == schema.NotIdentity {
			c.Default = schema.Expr(def)
		}
		t := get(schemaName, table)
		t.Columns = append(t.Columns, c)
	}
	return rows.Err()
}

// columnType builds the canonical type from what the catalog holds.
//
// format_type alone is not enough. It writes a type unqualified whenever the
// type is on the search path, so public.user_state comes back as user_state and
// would compare unequal to the declaration that named its schema. The catalog's
// own namespace is authoritative, and the rendering is used only for the
// modifier a declaration may have written — varchar(255) is not varchar.
func columnType(rendered, typeSchema, typeName, category, elemSchema, elemName string, fromExtension bool) schema.Type {
	t := schema.Type{Schema: typeSchema, Name: typeName}
	if category == "A" && elemName != "" {
		t = schema.Type{Schema: elemSchema, Name: elemName, Array: true}
	}
	// A built-in needs no schema: the canonical model leaves it empty so that
	// int8 and public.user_state stay distinguishable without a lookup.
	if t.Schema == "pg_catalog" {
		t.Schema = ""
	}
	// A type an extension brought is not a type this schema declares, and it is
	// written the way PostgreSQL writes it: geometry, citext, hstore — no
	// schema, because CREATE EXTENSION put it on the search path and every
	// declaration names it bare. Qualifying it would make a column declared
	// geometry(Point,4326) differ forever from the public.geometry(Point,4326)
	// the catalog holds, which is drift on a column nobody touched.
	if fromExtension {
		t.Schema = ""
	}
	// The rendering carries the modifier, which the catalog's type name does
	// not. It is taken only when it is one, so that a spelling difference
	// cannot creep back in through it.
	if mod := typeModifier(rendered); mod != "" {
		t.Name += mod
	}
	return t.Canonical()
}

// typeModifier returns the parenthesised modifier of a rendered type, if any.
func typeModifier(rendered string) string {
	name := strings.TrimSuffix(strings.TrimSpace(rendered), "[]")
	open := strings.IndexByte(name, '(')
	if open < 0 || !strings.HasSuffix(name, ")") {
		return ""
	}
	return name[open:]
}

func readConstraints(ctx context.Context, conn *pgx.Conn, searchPath []string, byName map[string]*schema.Table) error {
	rows, err := conn.Query(ctx, canonicalConstraintsQuery, searchPath, relkinds)
	if err != nil {
		return fmt.Errorf("reading constraints: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			schemaName, table, name, contype string
			cols                             []string
			refSchema, refTable              string
			refCols                          []string
			delType, updType                 string
			deferrable, deferred, validated  bool
			def                              string
		)
		if err := rows.Scan(&schemaName, &table, &name, &contype, &cols,
			&refSchema, &refTable, &refCols, &delType, &updType,
			&deferrable, &deferred, &validated, &def); err != nil {
			return fmt.Errorf("reading constraints: %w", err)
		}
		t, ok := byName[schemaName+"."+table]
		if !ok {
			continue
		}
		switch firstByte(contype) {
		case 'p':
			t.PrimaryKey = &schema.PrimaryKey{Name: name, Columns: cols}
		case 'u':
			t.Uniques = append(t.Uniques, schema.Unique{
				Name: name, Columns: cols, Constraint: true,
				NullsNotDistinct: strings.Contains(def, "NULLS NOT DISTINCT"),
			})
		case 'f':
			t.ForeignKeys = append(t.ForeignKeys, schema.ForeignKey{
				Name: name, Columns: cols,
				RefSchema: refSchema, RefTable: refTable, RefColumns: refCols,
				OnDelete: action(delType), OnUpdate: action(updType),
				Deferrable: deferrable, InitiallyDeferred: deferred,
				NotValid: !validated,
			})
		case 'c':
			t.Checks = append(t.Checks, schema.Check{
				Name:       name,
				Expression: checkExpression(def),
				NotValid:   !validated,
			})
		}
	}
	return rows.Err()
}

// action maps confdeltype/confupdtype to the canonical action.
func action(code string) schema.Action {
	switch firstByte(code) {
	case 'c':
		return schema.Cascade
	case 'r':
		return schema.Restrict
	case 'n':
		return schema.SetNull
	case 'd':
		return schema.SetDefault
	default:
		return schema.NoAction
	}
}

// checkExpression pulls the condition out of what pg_get_constraintdef rendered.
//
// PostgreSQL returns "CHECK ((amount >= 0)) NOT VALID"; the canonical model
// holds the condition alone, so that a constraint declared one way and read
// back the other compares equal.
func checkExpression(def string) schema.Expr {
	s := strings.TrimSpace(def)
	s = strings.TrimSuffix(s, " NOT VALID")
	const prefix = "CHECK "
	if !strings.HasPrefix(s, prefix) {
		return schema.Expr(s)
	}
	return schema.Expr(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s[len(prefix):], "("), ")")))
}

func readIndexes(ctx context.Context, conn *pgx.Conn, searchPath []string, byName map[string]*schema.Table) error {
	return readIndexesFor(ctx, conn, searchPath, byName, relkinds)
}

// readIndexesFor is readIndexes over a chosen set of relation kinds, so that a
// materialized view's indexes come through this reader rather than a copy of it.
func readIndexesFor(ctx context.Context, conn *pgx.Conn, searchPath []string, byName map[string]*schema.Table, kinds []string) error {
	rows, err := conn.Query(ctx, canonicalIndexesQuery, searchPath, kinds)
	if err != nil {
		return fmt.Errorf("reading indexes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			schemaName, table, name string
			unique                  bool
			keyCount                int16
			method                  string
			def                     string
			predicate               string
			ownedByConstraint       bool
		)
		if err := rows.Scan(&schemaName, &table, &name, &unique, &keyCount,
			&method, &def, &predicate, &ownedByConstraint); err != nil {
			return fmt.Errorf("reading indexes: %w", err)
		}
		t, ok := byName[schemaName+"."+table]
		if !ok {
			continue
		}
		// An index a constraint owns is not a schema object of its own: it
		// exists because the constraint does, and reporting both would make
		// every table look as though it had duplicate objects.
		if ownedByConstraint {
			continue
		}

		idx := schema.Index{
			Name:   name,
			Unique: unique,
			Method: method,
			Where:  schema.Expr(predicate),
		}
		keys, include := parseIndexDef(def, int(keyCount), t)
		idx.Columns = keys
		idx.Include = include

		// A unique index is an index. It was once recorded as a uniqueness
		// object instead, which put it somewhere a declared one never goes:
		// the two sides then disagreed about where the same object lived, and
		// every diff proposed to move it. It also threw away everything a
		// Unique cannot hold — the method, the sort order, the covering
		// columns, an expression key — for an object the model can describe in
		// full.
		t.Indexes = append(t.Indexes, idx)
	}
	return rows.Err()
}

// parseIndexDef reads the key list and the INCLUDE list out of what
// pg_get_indexdef rendered.
//
// The rendering is the only place an expression key, an operator class and a
// sort order all appear together, and PostgreSQL produces it consistently. What
// is parsed is the parenthesised list, which is a small enough grammar to read
// directly: anything more ambitious would be a SQL parser, which this package
// deliberately is not.
func parseIndexDef(def string, keyCount int, t *schema.Table) (keys []schema.IndexColumn, include []string) {
	body, rest, ok := cutBalanced(def, '(', ')')
	if !ok {
		return nil, nil
	}
	for _, part := range splitTopLevel(body) {
		keys = append(keys, parseIndexKey(part, t))
	}
	// Everything past the key list may hold INCLUDE and WHERE; only the first
	// is read here, since the predicate came back as its own column.
	if i := strings.Index(rest, "INCLUDE ("); i >= 0 {
		if inc, _, ok := cutBalanced(rest[i:], '(', ')'); ok {
			for _, part := range splitTopLevel(inc) {
				include = append(include, strings.Trim(strings.TrimSpace(part), `"`))
			}
		}
	}
	// INCLUDE columns are listed after the keys in some renderings; the key
	// count from the catalog is authoritative about where the keys stop.
	if keyCount > 0 && len(keys) > keyCount {
		for _, extra := range keys[keyCount:] {
			if extra.Name != "" {
				include = append(include, extra.Name)
			}
		}
		keys = keys[:keyCount]
	}
	return keys, include
}

// parseIndexKey reads one key: a column or expression, an optional operator
// class, and the sort order.
func parseIndexKey(s string, t *schema.Table) schema.IndexColumn {
	c := schema.IndexColumn{}
	rest := strings.TrimSpace(s)

	upper := strings.ToUpper(rest)
	switch {
	case strings.HasSuffix(upper, " NULLS FIRST"):
		c.Nulls = schema.NullsFirst
		rest = strings.TrimSpace(rest[:len(rest)-len(" NULLS FIRST")])
	case strings.HasSuffix(upper, " NULLS LAST"):
		c.Nulls = schema.NullsLast
		rest = strings.TrimSpace(rest[:len(rest)-len(" NULLS LAST")])
	}
	upper = strings.ToUpper(rest)
	switch {
	case strings.HasSuffix(upper, " DESC"):
		c.Direction = schema.Desc
		rest = strings.TrimSpace(rest[:len(rest)-len(" DESC")])
	case strings.HasSuffix(upper, " ASC"):
		rest = strings.TrimSpace(rest[:len(rest)-len(" ASC")])
	}

	// PostgreSQL renders NULLS LAST for an ascending key and NULLS FIRST for a
	// descending one only when they were asked for, so what is left here is the
	// key and possibly an operator class.
	if strings.HasPrefix(rest, "(") {
		if expr, after, ok := cutBalanced(rest, '(', ')'); ok {
			c.Expression = schema.Expr(strings.TrimSpace(expr))
			if op := strings.TrimSpace(after); op != "" {
				c.OpClass = strings.Trim(op, `"`)
			}
			return c
		}
	}
	name, opclass, hasOp := strings.Cut(rest, " ")
	quoted := strings.HasPrefix(strings.TrimSpace(name), `"`)
	trimmed := strings.Trim(strings.TrimSpace(name), `"`)
	if hasOp {
		c.OpClass = strings.Trim(strings.TrimSpace(opclass), `"`)
	}
	// PostgreSQL renders a function-call key without the parentheses it puts
	// round other expressions, so lower(title) arrives looking exactly like a
	// column name. The table decides: a key that names no column of it is an
	// expression, and calling it a column would produce an index over an
	// identifier that does not exist.
	if quoted || t == nil || columnExists(t, trimmed) {
		c.Name = trimmed
		return c
	}
	c.Expression = schema.Expr(strings.TrimSpace(rest))
	c.OpClass = ""
	return c
}

func columnExists(t *schema.Table, name string) bool {
	_, ok := t.Column(name)
	return ok
}

// cutBalanced returns the contents of the first balanced pair and what follows.
func cutBalanced(s string, open, close byte) (inside, rest string, ok bool) {
	start := strings.IndexByte(s, open)
	if start < 0 {
		return "", "", false
	}
	depth, inQuote := 0, false
	for i := start; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inQuote = !inQuote
		case inQuote:
		case s[i] == open:
			depth++
		case s[i] == close:
			depth--
			if depth == 0 {
				return s[start+1 : i], s[i+1:], true
			}
		}
	}
	return "", "", false
}

// splitTopLevel splits a comma-separated list, ignoring commas inside
// parentheses or quotes.
func splitTopLevel(s string) []string {
	var (
		out     []string
		depth   int
		inQuote bool
		start   int
	)
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inQuote = !inQuote
		case inQuote:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
		case s[i] == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}
