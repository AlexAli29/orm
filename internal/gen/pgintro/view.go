package pgintro

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/jackc/pgx/v5"
)

// Reading views and materialized views out of the catalog.
//
// PostgreSQL is the authority for all of it. The kind comes from pg_class.relkind
// rather than from anything about the name; the definition comes from
// pg_get_viewdef rather than from any attempt to recover the DDL somebody wrote;
// the dependencies come from pg_depend rather than from reading the SQL.
//
// Nothing here infers. A view is a view because the catalog says v.

// viewDefQuery reads each view's reconstructed definition, its options, and —
// for a materialized view — whether it currently holds data.
//
// pg_get_viewdef reconstructs the stored query for both ordinary and
// materialized views. What comes back is a correct statement of the definition
// and is not the original text: names are requalified, * is expanded, casts
// appear, comments are gone. That is exactly why it is useful for comparison
// and useless as a record of what was typed.
const viewDefQuery = `
SELECT c.oid,
       pg_get_viewdef(c.oid, true),
       COALESCE(c.reloptions, '{}')::text[],
       CASE WHEN c.relkind = 'm' THEN c.relispopulated ELSE true END,
       COALESCE(ct.option_text, '')
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN LATERAL (
    SELECT v.check_option AS option_text
    FROM information_schema.views v
    WHERE v.table_schema = n.nspname AND v.table_name = c.relname
) ct ON true
WHERE n.nspname = ANY($1) AND c.relkind = ANY(ARRAY['v','m'])` + notExtensionOwned + `
`

// viewDepQuery reads what each view's definition reads.
//
// The join goes through the view's rewrite rule, which is where PostgreSQL
// records a view's dependencies, and it is restricted to relations. A view
// depends on its own rule, on the types of its columns, on the functions it
// calls and on its owner; treating any of those as a relation edge would put
// nonsense in the migration order. Only pg_class dependencies of a relation
// kind that can be selected from are edges, and the view's dependency on itself
// is excluded.
const viewDepQuery = `
SELECT DISTINCT c.oid, dn.nspname, dc.relname, dc.relkind::text
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_rewrite r ON r.ev_class = c.oid
JOIN pg_depend d ON d.objid = r.oid AND d.classid = 'pg_rewrite'::regclass
JOIN pg_class dc ON dc.oid = d.refobjid AND d.refclassid = 'pg_class'::regclass
JOIN pg_namespace dn ON dn.oid = dc.relnamespace
WHERE n.nspname = ANY($1)
  AND c.relkind = ANY(ARRAY['v','m'])` + notExtensionOwned + `
  AND dc.oid <> c.oid
  AND dc.relkind = ANY(ARRAY['r','p','v','m','f'])
`

// loadViewDetail fills in everything that is true of a view and not of a table.
func loadViewDetail(ctx context.Context, conn *pgx.Conn, searchPath []string, byOID map[uint32]*model.PGTable) error {
	rows, err := conn.Query(ctx, viewDefQuery, searchPath)
	if err != nil {
		return fmt.Errorf("reading view definitions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var def string
		var opts []string
		var populated bool
		var checkOption string
		if err := rows.Scan(&oid, &def, &opts, &populated, &checkOption); err != nil {
			return fmt.Errorf("scanning view definitions: %w", err)
		}
		t, ok := byOID[oid]
		if !ok {
			continue
		}
		t.Definition = def
		t.Populated = populated
		t.Options = parseReloptions(opts)
		// information_schema reports NONE for a view with no check option, and
		// the absence of a row for a materialized view.
		if checkOption != "" && checkOption != "NONE" {
			t.Options = append(t.Options, model.PGViewOption{Name: "check_option", Value: checkOption})
		}
		slices.SortFunc(t.Options, func(a, b model.PGViewOption) int { return cmp.Compare(a.Name, b.Name) })
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading view definitions: %w", err)
	}
	rows.Close()

	deps, err := conn.Query(ctx, viewDepQuery, searchPath)
	if err != nil {
		return fmt.Errorf("reading view dependencies: %w", err)
	}
	defer deps.Close()
	for deps.Next() {
		var oid uint32
		var schemaName, name, kind string
		if err := deps.Scan(&oid, &schemaName, &name, &kind); err != nil {
			return fmt.Errorf("scanning view dependencies: %w", err)
		}
		t, ok := byOID[oid]
		if !ok {
			continue
		}
		t.DependsOn = append(t.DependsOn, model.PGRelationRef{
			Schema: schemaName, Name: name, Kind: firstByte(kind),
		})
	}
	if err := deps.Err(); err != nil {
		return fmt.Errorf("reading view dependencies: %w", err)
	}

	for _, t := range byOID {
		if !t.IsView() {
			continue
		}
		slices.SortFunc(t.DependsOn, func(a, b model.PGRelationRef) int {
			return cmp.Or(cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Name, b.Name))
		})
		t.DependsOn = slices.CompactFunc(t.DependsOn, func(a, b model.PGRelationRef) bool {
			return a.Schema == b.Schema && a.Name == b.Name
		})
		noteUnrepresented(t)
	}
	return nil
}

// parseReloptions turns PostgreSQL's text[] of name=value into pairs.
func parseReloptions(opts []string) []model.PGViewOption {
	var out []model.PGViewOption
	for _, o := range opts {
		name, value := o, ""
		for i := range len(o) {
			if o[i] == '=' {
				name, value = o[:i], o[i+1:]
				break
			}
		}
		out = append(out, model.PGViewOption{Name: name, Value: value})
	}
	return out
}

// knownViewOptions are the options this milestone represents and carries
// through a roundtrip unchanged.
var knownViewOptions = map[string]bool{
	"security_barrier": true,
	"security_invoker": true,
	"check_option":     true,
}

// noteUnrepresented records catalog metadata the schema model cannot express.
//
// It exists because the alternative is what other ORMs do: read a relation,
// write it back, and quietly drop whatever the model had no field for. On a
// view that means silently changing a security boundary — a security_invoker
// view read and recreated without it reads its base tables with the definer's
// privileges instead of the caller's. Reporting it is the whole point; the
// schema is allowed not to manage an option, and is not allowed to erase one.
func noteUnrepresented(t *model.PGTable) {
	for _, o := range t.Options {
		if !knownViewOptions[o.Name] {
			t.Unrepresented = append(t.Unrepresented,
				fmt.Sprintf("view option %s=%s", o.Name, o.Value))
		}
	}
	slices.Sort(t.Unrepresented)
}
