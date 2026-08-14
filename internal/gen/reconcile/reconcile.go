package reconcile

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/goscan"
	"github.com/AlexAli29/orm/internal/gen/model"
)

// Input is everything reconciliation compares.
type Input struct {
	Config   *config.Config
	Entities []*model.GoEntity
	Schema   *model.Schema
	// TagErrors are the malformed tags the scanner found. They become E021
	// findings here rather than in the scanner, so that every finding is
	// produced in one place.
	TagErrors []*goscan.TagError
}

// Run reconciles the entities against the schema and returns the mapping it
// proved together with everything it could not.
//
// The mapping is returned even when there are errors: a report that stops at
// the first problem forces the author to run the tool once per mistake.
func Run(in Input) (*model.Mapping, *diag.Report) {
	r := &reconciler{
		cfg:    in.Config,
		schema: in.Schema,
		report: &diag.Report{},
	}
	r.tagErrors(in.TagErrors)
	r.entityTier(in.Entities)
	r.columnTier()
	r.primaryKeyExposure()
	r.identifierCollisions()
	r.relationTier()
	r.unmappedColumns()
	return &model.Mapping{Entities: r.mappings, Schema: in.Schema}, r.report
}

type reconciler struct {
	cfg      *config.Config
	schema   *model.Schema
	report   *diag.Report
	mappings []*model.EntityMapping
	// byQualified resolves a relation target to the entity it names.
	byQualified map[string]*model.EntityMapping
	// relKeyCols records, per table, which columns any relation reads, so that
	// the unmapped-column report can say what an unmapped key column costs.
	relKeyCols map[*model.PGColumn][]string
}

func (r *reconciler) add(f diag.Finding) { r.report.Add(f) }

// tagErrors turns every malformed tag into E021.
func (r *reconciler) tagErrors(errs []*goscan.TagError) {
	for _, e := range errs {
		r.add(diag.Finding{
			Code:    diag.E021,
			Message: fmt.Sprintf("invalid orm tag on %s.%s", e.Entity, e.Field),
			Reason:  e.Err.Error(),
			Fix: "the mapping directives are column:<name>, fk:<constraint>, side:local, side:remote, type:<key> and -;" +
				" managed schema adds pk, identity, unique, default:<expr>, generated:<expr> and pgtype:<type>",
			Entity: e.Entity, Field: e.Field, GoType: e.Tag,
			Pos: e.Pos,
		})
	}
}

// entityTier resolves each entity to a table and reports the ways an entity can
// fail before any of its fields matter.
func (r *reconciler) entityTier(entities []*model.GoEntity) {
	r.byQualified = make(map[string]*model.EntityMapping, len(entities))
	byTable := make(map[string][]*model.EntityMapping)

	for _, e := range entities {
		table, ok := r.schema.Lookup(e.Table)
		if ok && !r.kindAgrees(e, table) {
			// The kind is wrong, and that decides everything else. Comparing a
			// view's columns against a table's, or looking for a materialized
			// view's indexes on an ordinary view, produces findings that are
			// true of nothing: the relation the project meant does not exist,
			// and a different one is standing where it should be. One accurate
			// finding beats a page of consequences.
			continue
		}
		if !ok {
			r.add(diag.Finding{
				Code:    diag.E016,
				Message: fmt.Sprintf("table %s does not exist", e.Table),
				Reason:  r.missingTableReason(e.Table),
				Fix:     fmt.Sprintf("create the table, correct the //orm:table directive on %s, or add its schema to schema.search_path", e.Display()),
				Entity:  e.Display(), Table: e.Table.String(),
				Pos: e.Marker,
			})
			continue
		}

		em := &model.EntityMapping{Entity: e, Table: table}
		r.mappings = append(r.mappings, em)
		r.byQualified[e.Qualified()] = em
		byTable[table.Qualified()] = append(byTable[table.Qualified()], em)
	}

	slices.SortFunc(r.mappings, func(a, b *model.EntityMapping) int {
		return cmp.Or(
			cmp.Compare(a.Entity.PkgPath, b.Entity.PkgPath),
			cmp.Compare(a.Entity.Name, b.Entity.Name),
		)
	})

	for _, qualified := range sortedKeys(byTable) {
		group := byTable[qualified]
		if len(group) < 2 {
			continue
		}
		slices.SortFunc(group, func(a, b *model.EntityMapping) int {
			return cmp.Compare(a.Entity.Qualified(), b.Entity.Qualified())
		})
		first := group[0].Entity
		for _, em := range group[1:] {
			r.add(diag.Finding{
				Code:    diag.E017,
				Message: fmt.Sprintf("%s and %s both map to %s", em.Entity.Display(), first.Display(), qualified),
				Reason:  "one table has one identity, one set of writable columns and one set of relations; two entities over it would disagree about all three",
				Fix:     fmt.Sprintf("map %s to its own table, or remove one of the two entities", em.Entity.Display()),
				Entity:  em.Entity.Display(), Table: qualified,
				Pos: em.Entity.Marker,
			})
		}
	}

	for _, em := range r.mappings {
		// A view has no primary key, and PostgreSQL allows none on one. Asking
		// for it would be a finding true of nothing — the fix it suggests is
		// impossible — and the reason it matters does not apply either: a view
		// is read-only here, so there is no update, delete or write API that
		// needs a row's identity.
		if em.Entity.Kind != model.RelTable {
			continue
		}
		if len(em.Table.PK) == 0 {
			r.add(diag.Finding{
				Code:    diag.E011,
				Message: fmt.Sprintf("%s has no primary key", em.Table.Qualified()),
				Reason:  "without a primary key a row has no identity, so it cannot be updated, deleted or related to",
				Fix:     fmt.Sprintf("add a primary key to %s", em.Table.Qualified()),
				Entity:  em.Entity.Display(), Table: em.Table.Qualified(),
				Pos: em.Entity.Marker,
			})
		}
	}
}

// missingTableReason says what was searched, which is usually the actual
// mistake: an unqualified name resolved against the wrong search path.
func (r *reconciler) missingTableReason(ref model.TableRef) string {
	if ref.Schema != "" {
		return fmt.Sprintf("no table named %s was introspected", ref)
	}
	return fmt.Sprintf("no table named %s was found in the search path %s", ref.Name, strings.Join(r.schema.SearchPath, ", "))
}

// columnTier maps each scalar field to a column and checks that its type can
// carry the column's values.
func (r *reconciler) columnTier() {
	for _, em := range r.mappings {
		byColumn := make(map[string][]int)
		for i := range em.Entity.Fields {
			f := &em.Entity.Fields[i]
			if f.Tags.Ignore || f.IsRelation() {
				continue
			}
			name := f.Tags.Column
			if name == "" {
				name = columnName(f.Name)
			}
			col := em.Table.Column(name)
			if col == nil {
				r.add(diag.Finding{
					Code:    diag.E001,
					Message: fmt.Sprintf("%s.%s has no column %s in %s", em.Entity.Display(), f.Name, name, em.Table.Qualified()),
					Reason:  r.noColumnReason(em, f, name),
					Fix:     "add the column, point the field at an existing one with orm:\"column:<name>\", or exclude it with orm:\"-\"",
					Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
					Table: em.Table.Qualified(), Column: name,
					Pos: f.Pos,
				})
				continue
			}
			byColumn[col.Name] = append(byColumn[col.Name], len(em.Cols))
			em.Cols = append(em.Cols, model.ColMapping{Field: f, Idx: i, Column: col})
			r.checkColumnType(em, f, col)
		}

		for _, name := range sortedKeys(byColumn) {
			idxs := byColumn[name]
			if len(idxs) < 2 {
				continue
			}
			first := em.Cols[idxs[0]].Field
			for _, idx := range idxs[1:] {
				cm := em.Cols[idx]
				r.add(diag.Finding{
					Code:    diag.E018,
					Message: fmt.Sprintf("%s.%s and %s.%s both map to %s", em.Entity.Display(), cm.Field.Name, em.Entity.Display(), first.Name, cm.Column.Qualified()),
					Reason:  "two fields over one column disagree on every write, and reading fills both with the same value",
					Fix:     fmt.Sprintf("point %s at another column with orm:\"column:<name>\", or exclude it with orm:\"-\"", cm.Field.Name),
					Entity:  em.Entity.Display(), Field: cm.Field.Name, GoType: cm.Field.Type.Src,
					Table: em.Table.Qualified(), Column: cm.Column.Name, PGType: cm.Column.Type.String(),
					Pos: cm.Field.Pos,
				})
			}
		}
	}
}

// noColumnReason distinguishes a name the author wrote from one the tool
// derived, because the fix is different.
func (r *reconciler) noColumnReason(em *model.EntityMapping, f *model.GoField, name string) string {
	if f.Tags.Column != "" {
		return fmt.Sprintf("the column: tag names %s, which %s does not have", name, em.Table.Qualified())
	}
	return fmt.Sprintf("the field name derives the column %s, which %s does not have", name, em.Table.Qualified())
}

// primaryKeyExposure reports an entity whose identity it cannot read back.
//
// Nothing in the column tier requires a primary key column to be mapped, and
// nothing in relation loading needs one either: an unmapped key column is
// selected as an auxiliary column when a loader needs it. The write path is what
// forces the issue. With a generated key, inserting succeeds and the RETURNING
// clause has nowhere to put the value, so the row cannot afterwards be
// addressed, updated or deleted.
func (r *reconciler) primaryKeyExposure() {
	for _, em := range r.mappings {
		var missing []string
		for _, pk := range em.Table.PK {
			if em.ColIdx(pk) < 0 {
				missing = append(missing, pk.Name)
			}
		}
		if len(missing) == 0 {
			continue
		}
		r.add(diag.Finding{
			Code:    diag.E023,
			Message: fmt.Sprintf("%s has no field for the primary key column %s of %s", em.Entity.Display(), strings.Join(missing, ", "), em.Table.Qualified()),
			Reason:  "an entity whose identity cannot be read back cannot be used with the write API: the generated key is discarded on insert, and update and delete have nothing to address",
			Fix:     fmt.Sprintf("add a field for %s to %s", strings.Join(missing, ", "), em.Entity.Display()),
			Entity:  em.Entity.Display(), Table: em.Table.Qualified(), Column: strings.Join(missing, ", "),
			Pos: em.Entity.Marker,
		})
	}
}

// identifierCollisions reports entities whose generated code would land in one
// directory under one name.
func (r *reconciler) identifierCollisions() {
	type key struct{ dir, name string }
	groups := make(map[key][]*model.EntityMapping)
	for _, em := range r.mappings {
		groups[key{dir: em.Entity.OutputDir, name: em.Entity.Name}] = append(groups[key{dir: em.Entity.OutputDir, name: em.Entity.Name}], em)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.dir, b.dir), cmp.Compare(a.name, b.name))
	})
	for _, k := range keys {
		group := groups[k]
		if len(group) < 2 {
			continue
		}
		slices.SortFunc(group, func(a, b *model.EntityMapping) int {
			return cmp.Compare(a.Entity.Qualified(), b.Entity.Qualified())
		})
		first := group[0].Entity
		for _, em := range group[1:] {
			r.add(diag.Finding{
				Code:    diag.E014,
				Message: fmt.Sprintf("%s and %s would generate the same identifiers", em.Entity.Qualified(), first.Qualified()),
				Reason:  fmt.Sprintf("both are named %s and both generate into %s", k.name, r.cfg.Rel(k.dir)),
				Fix:     "give the two entities separate output directories, or rename one of them",
				Entity:  em.Entity.Display(),
				Pos:     em.Entity.Marker,
			})
		}
	}
}

// unmappedColumns accounts for every column no field maps to.
//
// The distinction is not a matter of taste. A NOT NULL column with no default,
// no identity and no generation expression cannot be given a value through an
// entity that does not carry it, so a row can never be inserted: that is an
// error. Every other unmapped column merely means the entity ignores it, which
// is a choice the author may have made on purpose.
func (r *reconciler) unmappedColumns() {
	for _, em := range r.mappings {
		for _, col := range em.Table.Cols {
			if em.ColIdx(col) >= 0 {
				continue
			}
			if !col.Suppliable() {
				r.add(diag.Finding{
					Code:    diag.E002,
					Message: fmt.Sprintf("%s is NOT NULL with no default and is not mapped", col.Qualified()),
					Reason:  fmt.Sprintf("%s cannot supply a value for it, so no row can be inserted through this entity", em.Entity.Display()),
					Fix:     fmt.Sprintf("map the column on %s, give it a default, or make it nullable", em.Entity.Display()),
					Entity:  em.Entity.Display(),
					Table:   em.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
					Pos: em.Entity.Marker,
				})
				continue
			}
			if r.cfg.Strict.UnmappedColumns == config.Off {
				continue
			}
			r.add(diag.Finding{
				Code:     diag.W003,
				Severity: severity(r.cfg.Strict.UnmappedColumns),
				Message:  fmt.Sprintf("column %s is not mapped", col.Qualified()),
				Reason:   r.unmappedReason(em, col),
				Fix:      r.unmappedFix(em, col),
				Entity:   em.Entity.Display(),
				Table:    em.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
				Pos: em.Entity.Marker,
			})
		}
	}
}

// unmappedReason says what the missing mapping actually costs. When the column
// carries a relation's foreign key the consequence is specific, so it is stated
// specifically.
func (r *reconciler) unmappedReason(em *model.EntityMapping, col *model.PGColumn) string {
	if rels := r.relKeyCols[col]; len(rels) > 0 {
		return fmt.Sprintf("this column carries the foreign key for %s. Rows inserted through %s cannot set it, so the relationship is read-only from this entity",
			joinRelations(rels), em.Entity.Display())
	}
	switch {
	case col.IsGenerated():
		return fmt.Sprintf("%s computes the column, so it can be read but never written", col.Table.Qualified())
	case col.IsIdentity():
		return "the column is an identity column, so PostgreSQL supplies it, but its value cannot be read back"
	case col.HasDefault:
		return "the column has a default, so inserts succeed without it, but its value cannot be read back"
	default:
		return "the column is nullable, so inserts succeed without it, but its value cannot be read back"
	}
}

func (r *reconciler) unmappedFix(em *model.EntityMapping, col *model.PGColumn) string {
	if len(r.relKeyCols[col]) > 0 {
		return fmt.Sprintf("map it as a scalar field on %s if you need to write it", em.Entity.Display())
	}
	return fmt.Sprintf("add a field for %s to %s, or leave it unmapped on purpose and set strict.unmapped_columns: off", col.Name, em.Entity.Display())
}

// joinRelations renders an already-qualified list of relation names as prose.
func joinRelations(rels []string) string {
	switch len(rels) {
	case 0:
		return ""
	case 1:
		return rels[0]
	case 2:
		return rels[0] + " and " + rels[1]
	default:
		return strings.Join(rels[:len(rels)-1], ", ") + " and " + rels[len(rels)-1]
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
