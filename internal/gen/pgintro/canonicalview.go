package pgintro

import (
	"context"
	"fmt"

	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/jackc/pgx/v5"
)

// Reading views into the canonical schema.
//
// This is the actual half of reconciliation: what the database has, in the same
// value types the desired half is built in. It reuses the readers the tables
// use — the same column query, the same index query, the same expression
// parsing — because a materialized view's index is an index and a view column
// is a column. A reduced reader for either would be a second place for a
// covering column, a partial predicate or an operator class to be dropped, and
// the drop would be silent.

// canonicalViews reads the views and materialized views in the search path.
func canonicalViews(ctx context.Context, conn *pgx.Conn, searchPath []string) ([]schema.View, []schema.MaterializedView, error) {
	views := map[string]*schema.View{}
	mats := map[string]*schema.MaterializedView{}
	var order []string

	// Columns, through the same query tables use, restricted to view kinds.
	if err := readViewColumns(ctx, conn, searchPath, views, mats, &order); err != nil {
		return nil, nil, err
	}
	if len(order) == 0 {
		return nil, nil, nil
	}
	if err := readViewBodies(ctx, conn, searchPath, views, mats); err != nil {
		return nil, nil, err
	}
	if err := readViewDeps(ctx, conn, searchPath, views, mats); err != nil {
		return nil, nil, err
	}
	// Indexes, through the reader tables use. Only materialized views can carry
	// one, so only they are given a target to collect into.
	if err := readMatViewIndexes(ctx, conn, searchPath, mats); err != nil {
		return nil, nil, err
	}

	outV := make([]schema.View, 0, len(views))
	outM := make([]schema.MaterializedView, 0, len(mats))
	for _, key := range order {
		if v, ok := views[key]; ok {
			outV = append(outV, *v)
		}
		if m, ok := mats[key]; ok {
			outM = append(outM, *m)
		}
	}
	return outV, outM, nil
}

const canonicalViewColumnsQuery = `
SELECT n.nspname, c.relname, c.relkind::text, a.attname,
       format_type(a.atttypid, a.atttypmod),
       tn.nspname, t.typname, t.typcategory::text,
       COALESCE(en.nspname, ''), COALESCE(et.typname, ''),
       EXISTS (SELECT 1 FROM pg_depend dt
               WHERE dt.classid = 'pg_type'::regclass AND dt.objid = t.oid
                 AND dt.refclassid = 'pg_extension'::regclass AND dt.deptype = 'e')
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = a.atttypid
JOIN pg_namespace tn ON tn.oid = t.typnamespace
LEFT JOIN pg_type et ON et.oid = t.typelem
LEFT JOIN pg_namespace en ON en.oid = et.typnamespace
WHERE n.nspname = ANY($1) AND c.relkind = ANY(ARRAY['v','m'])
  AND a.attnum > 0 AND NOT a.attisdropped` + notExtensionOwned + `
ORDER BY n.nspname, c.relname, a.attnum`

func readViewColumns(ctx context.Context, conn *pgx.Conn, searchPath []string,
	views map[string]*schema.View, mats map[string]*schema.MaterializedView, order *[]string) error {
	rows, err := conn.Query(ctx, canonicalViewColumnsQuery, searchPath)
	if err != nil {
		return fmt.Errorf("reading view columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ns, rel, kind, col, rendered, typeSchema, typeName, category, elemSchema, elemName string
		var fromExtension bool
		if err := rows.Scan(&ns, &rel, &kind, &col, &rendered, &typeSchema, &typeName,
			&category, &elemSchema, &elemName, &fromExtension); err != nil {
			return fmt.Errorf("scanning view columns: %w", err)
		}
		key := ns + "." + rel
		if _, seen := views[key]; !seen {
			if _, seen := mats[key]; !seen {
				*order = append(*order, key)
				if kind == "m" {
					mats[key] = &schema.MaterializedView{Schema: ns, Name: rel}
				} else {
					views[key] = &schema.View{Schema: ns, Name: rel}
				}
			}
		}
		// A view column carries no NOT NULL in the catalog — PostgreSQL records
		// none, because it cannot say whether an expression yields NULL. So
		// every column here is nullable, which is the honest answer rather than
		// a guess in either direction.
		c := schema.Column{
			Name:     col,
			Type:     columnType(rendered, typeSchema, typeName, category, elemSchema, elemName, fromExtension),
			Nullable: true,
		}
		if m, ok := mats[key]; ok {
			m.Columns = append(m.Columns, c)
			continue
		}
		views[key].Columns = append(views[key].Columns, c)
	}
	return rows.Err()
}

func readViewBodies(ctx context.Context, conn *pgx.Conn, searchPath []string,
	views map[string]*schema.View, mats map[string]*schema.MaterializedView) error {
	rows, err := conn.Query(ctx, viewDefQuery, searchPath)
	if err != nil {
		return fmt.Errorf("reading view definitions: %w", err)
	}
	defer rows.Close()
	type row struct {
		def         string
		opts        []string
		populated   bool
		checkOption string
	}
	byOID := map[uint32]row{}
	var oids []uint32
	for rows.Next() {
		var oid uint32
		var r row
		if err := rows.Scan(&oid, &r.def, &r.opts, &r.populated, &r.checkOption); err != nil {
			return fmt.Errorf("scanning view definitions: %w", err)
		}
		byOID[oid] = r
		oids = append(oids, oid)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	// The definition query keys by OID; names are what the schema uses.
	names, err := conn.Query(ctx, `SELECT c.oid, n.nspname, c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ANY($1) AND c.relkind = ANY(ARRAY['v','m'])`+notExtensionOwned, searchPath)
	if err != nil {
		return err
	}
	defer names.Close()
	for names.Next() {
		var oid uint32
		var ns, rel string
		if err := names.Scan(&oid, &ns, &rel); err != nil {
			return err
		}
		r, ok := byOID[oid]
		if !ok {
			continue
		}
		key := ns + "." + rel
		// Only the canonical half is filled in: this is what the database says,
		// and the project's own SQL is not here to compare against it.
		def := schema.Definition{Canonical: r.def}
		opts := parseReloptions(r.opts)
		var vopts []schema.ViewOption
		for _, o := range opts {
			vopts = append(vopts, schema.ViewOption{Name: o.Name, Value: o.Value})
		}
		if r.checkOption != "" && r.checkOption != "NONE" {
			vopts = append(vopts, schema.ViewOption{Name: "check_option", Value: r.checkOption})
		}
		if m, ok := mats[key]; ok {
			// A materialized view carries no view options: PostgreSQL exposes
			// security_barrier, security_invoker and the check option on
			// ordinary views only, and inventing symmetry would mean comparing
			// fields that cannot exist.
			m.Definition, m.Populated = def, r.populated
			continue
		}
		if v, ok := views[key]; ok {
			v.Definition, v.Options = def, vopts
		}
	}
	return names.Err()
}

func readViewDeps(ctx context.Context, conn *pgx.Conn, searchPath []string,
	views map[string]*schema.View, mats map[string]*schema.MaterializedView) error {
	rows, err := conn.Query(ctx, `
SELECT DISTINCT n.nspname, c.relname, dn.nspname, dc.relname, dc.relkind::text
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_rewrite r ON r.ev_class = c.oid
JOIN pg_depend d ON d.objid = r.oid AND d.classid = 'pg_rewrite'::regclass
JOIN pg_class dc ON dc.oid = d.refobjid AND d.refclassid = 'pg_class'::regclass
JOIN pg_namespace dn ON dn.oid = dc.relnamespace
WHERE n.nspname = ANY($1)
  AND c.relkind = ANY(ARRAY['v','m'])`+notExtensionOwned+`
  AND dc.oid <> c.oid
  AND dc.relkind = ANY(ARRAY['r','p','v','m','f'])`, searchPath)
	if err != nil {
		return fmt.Errorf("reading view dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ns, rel, dns, drel, dkind string
		if err := rows.Scan(&ns, &rel, &dns, &drel, &dkind); err != nil {
			return err
		}
		ref := schema.RelationRef{Schema: dns, Name: drel, KindKnown: true}
		switch dkind {
		case "v":
			ref.Kind = schema.KindView
		case "m":
			ref.Kind = schema.KindMaterializedView
		default:
			ref.Kind = schema.KindTable
		}
		key := ns + "." + rel
		if m, ok := mats[key]; ok {
			m.DependsOn = append(m.DependsOn, ref)
			continue
		}
		if v, ok := views[key]; ok {
			v.DependsOn = append(v.DependsOn, ref)
		}
	}
	return rows.Err()
}

// readMatViewIndexes reads materialized-view indexes through the same query and
// the same definition parsing tables use.
func readMatViewIndexes(ctx context.Context, conn *pgx.Conn, searchPath []string,
	mats map[string]*schema.MaterializedView) error {
	if len(mats) == 0 {
		return nil
	}
	// The reader works over schema.Table, so each materialized view lends one
	// for the duration. That is the reuse: the index model, the definition
	// parsing and the expression handling are the table path's, unchanged.
	byName := make(map[string]*schema.Table, len(mats))
	for key, m := range mats {
		byName[key] = &schema.Table{Schema: m.Schema, Name: m.Name, Columns: m.Columns}
	}
	if err := readIndexesFor(ctx, conn, searchPath, byName, []string{"m"}); err != nil {
		return err
	}
	for key, m := range mats {
		m.Indexes = byName[key].Indexes
	}
	return nil
}
