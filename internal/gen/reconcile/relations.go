package reconcile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/model"
)

// relationTier resolves every orm.One and orm.Many against the catalog.
//
// Direction is never declared. The Go field says how many related rows there
// are; PostgreSQL says which table holds the foreign key. Only when the catalog
// is genuinely ambiguous — a table with two foreign keys to the same target, or
// a self-reference that could be read either way — does the author have to pin
// the choice with fk: and side:.
func (r *reconciler) relationTier() {
	r.relKeyCols = make(map[*model.PGColumn][]string)
	for _, em := range r.mappings {
		for i := range em.Entity.Fields {
			f := &em.Entity.Fields[i]
			if f.Tags.Ignore || !f.IsRelation() {
				continue
			}
			r.relation(em, f, i)
		}
	}
	for col := range r.relKeyCols {
		slices.Sort(r.relKeyCols[col])
	}
}

func (r *reconciler) relation(em *model.EntityMapping, f *model.GoField, idx int) {
	target, ok := r.byQualified[f.Rel.Target]
	if !ok {
		r.add(diag.Finding{
			Code:    diag.E020,
			Message: fmt.Sprintf("%s.%s targets %s, which is not a mapped entity", em.Entity.Display(), f.Name, shortName(f.Rel.Target)),
			Reason:  fmt.Sprintf("%s carries no //orm:table directive, or its own reconciliation failed, so there is no table to relate to", f.Rel.Target),
			Fix:     fmt.Sprintf("mark %s with //orm:table, or remove the relation", shortName(f.Rel.Target)),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src, Relation: f.Name,
			Pos: f.Pos,
		})
		return
	}

	// A relation is loaded by code generated into the entity's own package, and
	// that code needs the target's descriptors — which are generated into the
	// target's package, unexported. So a relation that crosses a package
	// boundary cannot be generated today.
	//
	// It is reported here, during reconciliation, rather than only when code is
	// emitted. The difference is when a developer finds out: reconciliation is
	// what `orm check` runs, so this arrives before a migration has been
	// written for the foreign key the relation implies, instead of after it has
	// been applied to a database.
	//
	// The relation in the other direction cannot be declared at all: with the
	// current model, where an entity names its target's Go type, two mutually
	// dependent entity packages would form a Go import cycle.
	if target.Entity.PkgPath != em.Entity.PkgPath {
		r.add(diag.Finding{
			Code:    diag.E024,
			Message: fmt.Sprintf("%s.%s targets %s, which is in another package", em.Entity.Display(), f.Name, target.Entity.Display()),
			Reason:  "a relation is loaded through the target's generated descriptors, and those are generated into the target's own package where this one cannot reach them",
			Fix:     fmt.Sprintf("declare %s and %s in one package, or drop the relation and read the foreign key column directly", em.Entity.Display(), target.Entity.Display()),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src, Relation: f.Name,
			Pos: f.Pos,
		})
		return
	}

	c := relCandidates{
		allLocal:  candidates(em.Table.FKs, target.Table),
		allRemote: candidates(target.Table.FKs, em.Table),
	}
	local, remote := c.allLocal, c.allRemote

	if name := f.Tags.FK; name != "" {
		local, remote = byName(local, name), byName(remote, name)
		if len(local) == 0 && len(remote) == 0 {
			r.add(diag.Finding{
				Code:    diag.E008,
				Message: fmt.Sprintf("%s.%s names the constraint %s, which does not relate %s and %s", em.Entity.Display(), f.Name, name, em.Table.Qualified(), target.Table.Qualified()),
				Reason:  r.candidateReason(em, target),
				Fix:     "name one of the constraints listed above, or add the missing foreign key",
				Entity:  em.Entity.Display(), Field: f.Name, Relation: f.Name, Constraint: name,
				Table: em.Table.Qualified(),
				Pos:   f.Pos,
			})
			return
		}
	}
	// A side: tag narrows the candidates — except on a self-reference, where
	// both lists hold the very same constraints and dropping one would leave
	// nothing to resolve. There the tag is applied to the direction instead,
	// inside resolveSide.
	if f.Tags.HasSide && em.Table != target.Table {
		if f.Tags.Side == model.FKLocal {
			remote = nil
		} else {
			local = nil
		}
	}
	c.local, c.remote = local, remote

	fk, side, ok := r.resolveSide(em, target, f, c)
	if !ok {
		return
	}
	if !r.validate(em, target, f, fk, side) {
		return
	}

	rm := model.RelMapping{
		Field:       f,
		Cardinality: f.Rel.Cardinality,
		FKSide:      side,
		FK:          fk,
		Idx:         idx,
		Target:      target,
	}
	// KeyCols always name columns on the declaring entity's table and
	// TargetCols always name columns on the target's, whichever side declares
	// the constraint. A consumer matches entityRow[KeyCols] against
	// targetRow[TargetCols] and never has to branch on FKSide.
	if side == model.FKLocal {
		rm.KeyCols = r.keyCols(em, fk.Cols)
		rm.TargetCols = r.keyCols(target, fk.RefCols)
	} else {
		rm.KeyCols = r.keyCols(em, fk.RefCols)
		rm.TargetCols = r.keyCols(target, fk.Cols)
	}
	em.Rels = append(em.Rels, rm)

	label := em.Entity.Display() + "." + f.Name
	r.noteKeyCols(rm.KeyCols, label)
	r.noteKeyCols(rm.TargetCols, label)
}

// relCandidates holds the foreign keys that could back a relation, both as the
// catalog offers them and as the field's tags narrowed them.
//
// The unnarrowed lists are kept because they are what makes a failure
// explicable: "there is no foreign key between these tables" and "the only
// foreign key is on the side your tag excluded" are different problems with
// different fixes, and only the second one is the author's tag.
type relCandidates struct {
	// local and remote are what resolution actually considers.
	local, remote []*model.PGForeignKey
	// allLocal and allRemote are what the catalog offered before any fk: or
	// side: tag narrowed them.
	allLocal, allRemote []*model.PGForeignKey
}

// resolveSide picks the one foreign key that backs the relation.
func (r *reconciler) resolveSide(em, target *model.EntityMapping, f *model.GoField, c relCandidates) (*model.PGForeignKey, model.FKSide, bool) {
	card := f.Rel.Cardinality
	local, remote := c.local, c.remote

	// A self-reference sees the same constraints from both sides, so counting
	// them cannot decide anything. Cardinality does: one row of the parent is
	// reached through the table's own key, many rows of the child through
	// theirs.
	if em.Table == target.Table {
		all := local
		if len(all) == 0 {
			r.noCandidate(em, target, f, c)
			return nil, 0, false
		}
		if len(all) > 1 {
			r.ambiguous(em, target, f, all, "the table has more than one foreign key to itself")
			return nil, 0, false
		}
		side := model.FKRemote
		if card == model.CardOne {
			side = model.FKLocal
		}
		if f.Tags.HasSide {
			side = f.Tags.Side
		}
		return all[0], side, true
	}

	if card == model.CardMany {
		switch {
		case len(remote) == 1:
			return remote[0], model.FKRemote, true
		case len(remote) > 1:
			r.ambiguous(em, target, f, remote, fmt.Sprintf("%s has more than one foreign key to %s", target.Table.Qualified(), em.Table.Qualified()))
			return nil, 0, false
		case len(local) > 0:
			// The only foreign key runs the wrong way: many rows cannot hang
			// off a key the declaring table holds a single copy of.
			r.add(diag.Finding{
				Code:    diag.E019,
				Message: fmt.Sprintf("%s.%s is orm.Many but the foreign key is on %s", em.Entity.Display(), f.Name, em.Table.Qualified()),
				Reason:  fmt.Sprintf("%s holds one %s per row, so it can reference at most one %s", em.Table.Qualified(), strings.Join(fkColNames(local[0]), ", "), target.Entity.Display()),
				Fix:     fmt.Sprintf("declare the field as orm.One[%s], or move the foreign key to %s", target.Entity.Name, target.Table.Qualified()),
				Entity:  em.Entity.Display(), Field: f.Name, Relation: f.Name, Constraint: local[0].Name,
				Table: em.Table.Qualified(),
				Pos:   f.Pos,
			})
			return nil, 0, false
		default:
			r.noCandidate(em, target, f, c)
			return nil, 0, false
		}
	}

	switch {
	case len(local) == 1 && len(remote) == 0:
		return local[0], model.FKLocal, true
	case len(local) == 0 && len(remote) == 1:
		return remote[0], model.FKRemote, true
	case len(local) == 0 && len(remote) == 0:
		r.noCandidate(em, target, f, c)
		return nil, 0, false
	default:
		r.ambiguous(em, target, f, append(slices.Clone(local), remote...),
			fmt.Sprintf("%d foreign keys could back it: %d on %s and %d on %s",
				len(local)+len(remote), len(local), em.Table.Qualified(), len(remote), target.Table.Qualified()))
		return nil, 0, false
	}
}

// validate applies the uniqueness rule that distinguishes a has-one from a
// has-many, and only that rule.
//
// A belongs-to imposes no requirement at all: many posts may share an author,
// and demanding a unique index on posts.author_id would reject the most common
// relation in any schema. A has-one does impose one, because it claims that at
// most one row of the other table points back, and only a total unique index
// makes that true.
func (r *reconciler) validate(em, target *model.EntityMapping, f *model.GoField, fk *model.PGForeignKey, side model.FKSide) bool {
	if f.Rel.Cardinality == model.CardMany && side == model.FKLocal {
		r.add(diag.Finding{
			Code:    diag.E019,
			Message: fmt.Sprintf("%s.%s is orm.Many but %s is on %s", em.Entity.Display(), f.Name, fk.Name, em.Table.Qualified()),
			Reason:  "a local foreign key holds one value per row, so it can never produce many related rows",
			Fix:     fmt.Sprintf("declare the field as orm.One[%s], or use the foreign key on %s with side:remote", target.Entity.Name, target.Table.Qualified()),
			Entity:  em.Entity.Display(), Field: f.Name, Relation: f.Name, Constraint: fk.Name,
			Table: em.Table.Qualified(),
			Pos:   f.Pos,
		})
		return false
	}
	if f.Rel.Cardinality != model.CardOne || side != model.FKRemote {
		return true
	}
	if provesUnique(fk) {
		return true
	}

	cols := strings.Join(fkColNames(fk), ", ")
	r.add(diag.Finding{
		Code:    diag.E010,
		Message: fmt.Sprintf("%s.%s is orm.One but %s(%s) is not unique", em.Entity.Display(), f.Name, fk.Table.Qualified(), cols),
		Reason:  uniqueReason(fk),
		Fix:     fmt.Sprintf("add a total unique constraint on %s(%s), or declare the field as orm.Many[%s]", fk.Table.Qualified(), cols, target.Entity.Name),
		Entity:  em.Entity.Display(), Field: f.Name, Relation: f.Name, Constraint: fk.Name,
		Table: fk.Table.Qualified(), Column: cols,
		Pos: f.Pos,
	})
	return false
}

// provesUnique reports whether some total unique index on the referencing table
// guarantees at most one row per referenced key.
//
// A unique index over a subset of the foreign key's columns is enough: if
// user_id alone is unique then (tenant_id, user_id) certainly is. A partial or
// expression index is never enough, whatever columns it names.
func provesUnique(fk *model.PGForeignKey) bool {
	for _, u := range fk.Table.Uniques {
		if !u.Total() {
			continue
		}
		covered := true
		for _, c := range u.Cols {
			if !slices.Contains(fk.Cols, c) {
				covered = false
				break
			}
		}
		if covered {
			return true
		}
	}
	return false
}

// uniqueReason names the index the author probably thought was sufficient.
func uniqueReason(fk *model.PGForeignKey) string {
	for _, u := range fk.Table.Uniques {
		if u.Total() {
			continue
		}
		for _, c := range u.Cols {
			if !slices.Contains(fk.Cols, c) {
				continue
			}
			switch {
			case u.Partial:
				return fmt.Sprintf("%s is a partial unique index, so it says nothing about the rows its WHERE clause excludes; a to-one relation would silently drop rows", u.Name)
			case u.Expression:
				return fmt.Sprintf("%s indexes an expression rather than the column itself, so it does not constrain the column", u.Name)
			}
		}
	}
	return "nothing stops two rows from referencing the same one, so a to-one relation would silently pick whichever came back first"
}

// keyCols pairs each relation key column with its mapped field, or with -1.
//
// A -1 is not a failure. The column is the source of truth and a loader can
// always select it; a mapped field only means the loader can read the key from
// an already-scanned entity instead. The order is the constraint's own ordinality
// and is never sorted: it is what pairs the two sides of a composite key.
func (r *reconciler) keyCols(owner *model.EntityMapping, cols []*model.PGColumn) []model.RelKeyCol {
	out := make([]model.RelKeyCol, 0, len(cols))
	for _, c := range cols {
		out = append(out, model.RelKeyCol{Column: c, FieldIdx: owner.ColIdx(c)})
	}
	return out
}

// noteKeyCols records which relations read an unmapped column, so that the
// unmapped-column report can say what the missing field costs.
func (r *reconciler) noteKeyCols(keys []model.RelKeyCol, label string) {
	for _, k := range keys {
		if k.Mapped() {
			continue
		}
		if !slices.Contains(r.relKeyCols[k.Column], label) {
			r.relKeyCols[k.Column] = append(r.relKeyCols[k.Column], label)
		}
	}
}

// noCandidate reports a relation nothing in the catalog can back.
//
// When a side: tag is what emptied the candidate list, the schema is fine and
// the tag is wrong, so the finding says so rather than claiming a foreign key
// that plainly exists does not.
func (r *reconciler) noCandidate(em, target *model.EntityMapping, f *model.GoField, c relCandidates) {
	if excluded := r.sideExcluded(f, c); len(excluded) > 0 {
		wanted, other := em.Table.Qualified(), target.Table.Qualified()
		if f.Tags.Side == model.FKRemote {
			wanted, other = other, wanted
		}
		r.add(diag.Finding{
			Code:    diag.E008,
			Message: fmt.Sprintf("%s.%s asks for side:%s, but %s has no foreign key to %s", em.Entity.Display(), f.Name, f.Tags.Side, wanted, other),
			Reason:  fmt.Sprintf("the foreign key between them is %s, on %s, which side:%s excludes", strings.Join(constraintNames(excluded), ", "), other, f.Tags.Side),
			Fix:     fmt.Sprintf("drop the side: tag and let the catalog decide, or write side:%s", opposite(f.Tags.Side)),
			Entity:  em.Entity.Display(), Field: f.Name, Relation: f.Name,
			Table: em.Table.Qualified(),
			Pos:   f.Pos,
		})
		return
	}
	r.add(diag.Finding{
		Code:    diag.E008,
		Message: fmt.Sprintf("%s.%s has no foreign key between %s and %s", em.Entity.Display(), f.Name, em.Table.Qualified(), target.Table.Qualified()),
		Reason:  "a relation is a fact about the schema; without a foreign key there is nothing to derive it from",
		Fix:     fmt.Sprintf("add a foreign key between %s and %s", em.Table.Qualified(), target.Table.Qualified()),
		Entity:  em.Entity.Display(), Field: f.Name, Relation: f.Name,
		Table: em.Table.Qualified(),
		Pos:   f.Pos,
	})
}

// sideExcluded returns the candidates a side: tag removed, or nil when the tag
// is not what left the relation without one.
func (r *reconciler) sideExcluded(f *model.GoField, c relCandidates) []*model.PGForeignKey {
	if !f.Tags.HasSide {
		return nil
	}
	if f.Tags.Side == model.FKLocal {
		return c.allRemote
	}
	return c.allLocal
}

func opposite(s model.FKSide) model.FKSide {
	if s == model.FKLocal {
		return model.FKRemote
	}
	return model.FKLocal
}

func (r *reconciler) ambiguous(em, target *model.EntityMapping, f *model.GoField, cands []*model.PGForeignKey, why string) {
	names := constraintNames(cands)
	fix := fmt.Sprintf("pin the constraint with orm:\"fk:%s\"", names[0])
	if em.Table == target.Table {
		fix += ", and add side:local or side:remote if the direction is also ambiguous"
	}
	r.add(diag.Finding{
		Code:    diag.E009,
		Message: fmt.Sprintf("%s.%s is ambiguous: %s", em.Entity.Display(), f.Name, strings.Join(names, ", ")),
		Reason:  why,
		Fix:     fix,
		Entity:  em.Entity.Display(), Field: f.Name, Relation: f.Name,
		Table: em.Table.Qualified(),
		Pos:   f.Pos,
	})
}

// candidateReason lists what could have backed the relation, which is the
// information the author needs when a fk: tag names the wrong constraint.
func (r *reconciler) candidateReason(em, target *model.EntityMapping) string {
	names := constraintNames(append(candidates(em.Table.FKs, target.Table), candidates(target.Table.FKs, em.Table)...))
	if len(names) == 0 {
		return fmt.Sprintf("no foreign key relates %s and %s", em.Table.Qualified(), target.Table.Qualified())
	}
	return "the constraints that do relate them are " + strings.Join(names, ", ")
}

// candidates returns the foreign keys among fks that reference to.
func candidates(fks []*model.PGForeignKey, to *model.PGTable) []*model.PGForeignKey {
	var out []*model.PGForeignKey
	for _, fk := range fks {
		if fk.RefTable == to {
			out = append(out, fk)
		}
	}
	return out
}

func byName(fks []*model.PGForeignKey, name string) []*model.PGForeignKey {
	var out []*model.PGForeignKey
	for _, fk := range fks {
		if fk.Name == name {
			out = append(out, fk)
		}
	}
	return out
}

func constraintNames(fks []*model.PGForeignKey) []string {
	names := make([]string, 0, len(fks))
	for _, fk := range fks {
		if !slices.Contains(names, fk.Name) {
			names = append(names, fk.Name)
		}
	}
	slices.Sort(names)
	return names
}

func fkColNames(fk *model.PGForeignKey) []string {
	names := make([]string, 0, len(fk.Cols))
	for _, c := range fk.Cols {
		names = append(names, c.Name)
	}
	return names
}
