package emit

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/model"
)

// Relation code generation.
//
// Everything here comes from the mapping reconciliation proved: which foreign
// key backs a relation, which side declares it, and which columns pair with
// which, in the order the constraint declares them. Nothing is inferred from a
// field name or a naming convention.
//
// Every relation gets what both loading strategies need, because which one it
// uses is decided per query rather than per relation. A to-one relation with no
// options folds into the root statement with a LEFT JOIN, so it needs somewhere
// to put a row that might not be there; the same relation with a filter loads
// separately, so it also needs a way to read the parent keys, scan a target row
// and attach it. A to-many relation only ever loads separately.
//
// Keys can only be read from the entity when the entity maps them. A relation
// whose parent key columns are unmapped is legitimate and generates no key
// reader; the runtime then knows it cannot batch that relation and keeps it in
// the join.

// relFuncName is the generated binder or loader for one relation.
func relBindName(entity, rel string) string   { return unexport(entity) + rel + "Bind" }
func relLoadName(entity, rel string) string   { return unexport(entity) + rel + "Load" }
func relAttachName(entity, rel string) string { return unexport(entity) + rel + "Attach" }
func relRefsName(entity, rel string) string   { return unexport(entity) + rel + "Refs" }

// The buffer a relation reads its parent keys into when the entity does not map
// them, and the columns behind it.
func relAuxTypeName(entity, rel string) string    { return unexport(entity) + rel + "Aux" }
func relAuxCtorName(entity, rel string) string    { return "new" + entity + rel + "Aux" }
func relAuxColumnsName(entity, rel string) string { return unexport(entity) + rel + "AuxColumns" }

// nullsTypeName is the mirror of an entity whose every column can hold NULL.
func nullsTypeName(entity string) string       { return unexport(entity) + "Nulls" }
func nullsDestName(entity string) string       { return unexport(entity) + "NullDest" }
func nullsRestoreName(entity string) string    { return unexport(entity) + "FromNulls" }
func relKeysName(entity, rel string) string    { return unexport(entity) + rel + "Keys" }
func relColumnsName(entity, rel string) string { return unexport(entity) + rel + "Columns" }

// relDestName is the scan destinations of one relation target, generated once
// per target rather than once per relation that points at it.
func relDestName(entity string) string { return unexport(entity) + "RelDest" }

// foldTargets returns the entities that are the target of a to-one relation
// somewhere in the package, sorted, so that the NULL mirror is generated once
// for each and only for the ones that need it.
func (c *pkgContext) foldTargets() []*model.EntityMapping {
	need := make(map[string]bool)
	for _, em := range c.entities {
		for _, rel := range em.Rels {
			if rel.Cardinality == model.CardOne {
				need[rel.Target.Entity.Name] = true
			}
		}
	}
	var out []*model.EntityMapping
	for _, em := range c.entities {
		if need[em.Entity.Name] {
			out = append(out, em)
		}
	}
	return out
}

// relTargets returns every entity some relation in the package points at, in
// declaration order. Each needs one set of scan destinations, however many
// relations reach it.
func (c *pkgContext) relTargets() []*model.EntityMapping {
	need := make(map[string]bool)
	for _, em := range c.entities {
		for _, rel := range em.Rels {
			need[rel.Target.Entity.Name] = true
		}
	}
	var out []*model.EntityMapping
	for _, em := range c.entities {
		if need[em.Entity.Name] {
			out = append(out, em)
		}
	}
	return out
}

// relations renders the relation descriptors' supporting code.
func (c *pkgContext) relations() (File, error) {
	// A package with no relations gets no file. An empty one would be a
	// permanent, contentless diff in every project that does not use them.
	if !c.hasRelations() {
		return File{}, nil
	}
	b := newBuilder(c.name, c.path)
	b.imports(runtimeImport)

	// The mirror lets a LEFT JOIN that matched nothing be scanned at all. Every
	// target column arrives as NULL in that case, and a field that cannot hold
	// NULL would fail to scan rather than report an absent relation.
	for _, em := range c.foldTargets() {
		entity := em.Entity.Name
		typeName := nullsTypeName(entity)

		b.line("// %s mirrors %s with every column able to hold NULL.", typeName, entity)
		b.line("//")
		b.line("// A LEFT JOIN that matched nothing returns NULL for every column of the")
		b.line("// target, including the ones the entity declares as plain values. Scanning")
		b.line("// straight into %s would fail on those rather than report an absent", entity)
		b.line("// relation, so the row lands here first.")
		b.line("type %s struct {", typeName)
		for _, cm := range em.Cols {
			b.line("\t%s %s", cm.Field.Name, mirrorType(cm.Field.Type))
			// The mirror writes the declared type, whose imports are not
			// always the value type's: sql.Null[string] names database/sql
			// where the string behind it names nothing.
			b.imports(cm.Field.Type.SrcRefs...)
		}
		b.line("}")
		b.blank()

		b.line("// %s returns scan destinations for every column of %s.", nullsDestName(entity), em.Table.Qualified())
		b.line("func %s(n *%s) []any {", nullsDestName(entity), typeName)
		b.line("\treturn []any{")
		for _, cm := range em.Cols {
			b.line("\t\t&n.%s,", cm.Field.Name)
		}
		b.line("\t}")
		b.line("}")
		b.blank()

		present, err := presenceCheck(em)
		if err != nil {
			return File{}, err
		}
		b.line("// %s rebuilds %s from a scanned row, reporting whether there was a row", nullsRestoreName(entity), entity)
		b.line("// at all.")
		b.line("//")
		b.line("// Presence is decided by the primary key, which PostgreSQL guarantees is")
		b.line("// not NULL for a real row. Testing some other column would confuse a row")
		b.line("// whose fields happen to be NULL with the absence of a row.")
		b.line("func %s(n *%s) (%s, bool) {", nullsRestoreName(entity), typeName, entity)
		b.line("\tif %s {", present)
		b.line("\t\treturn %s{}, false", entity)
		b.line("\t}")
		b.line("\treturn %s{", entity)
		for _, cm := range em.Cols {
			b.line("\t\t%s: %s,", cm.Field.Name, restoreExpr(cm.Field))
		}
		b.line("\t}, true")
		b.line("}")
		b.blank()
	}

	// Every relation may end up loaded in a statement of its own, so every
	// target needs a way to be scanned out of one. It is written once per
	// target: two relations to users read a user the same way.
	for _, em := range c.relTargets() {
		entity := em.Entity.Name
		b.line("// %s returns scan destinations for a %s loaded as a related row.", relDestName(entity), entity)
		b.line("//")
		b.line("// A related row loaded in a statement of its own is read from the target")
		b.line("// table directly, so no column of it is NULL for want of a matching row and")
		b.line("// the entity's own scanner is enough.")
		b.line("func %s(t *%s) []any {", relDestName(entity), entity)
		b.line("\tdest := make([]any, %d)", len(em.Cols))
		b.line("\tfor i := range dest {")
		b.line("\t\tdest[i] = %s(t, i)", destFuncName(entity))
		b.line("\t}")
		b.line("\treturn dest")
		b.line("}")
		b.blank()
	}

	for _, em := range c.entities {
		for _, rel := range em.Rels {
			if err := c.relation(b, em, rel); err != nil {
				return File{}, err
			}
		}
	}
	return b.file(filepath.Join(c.dir, relFile))
}

// hasRelations reports whether any entity in the package declares one.
func (c *pkgContext) hasRelations() bool {
	for _, em := range c.entities {
		if len(em.Rels) > 0 {
			return true
		}
	}
	return false
}

// relation renders the supporting code for one relation.
func (c *pkgContext) relation(b *builder, em *model.EntityMapping, rel model.RelMapping) error {
	entity := em.Entity.Name
	target := rel.Target.Entity
	name := rel.Field.Name

	// Reconciliation reports this as E024 and generation is refused before it
	// gets here, so reaching this point means the two disagree. It stays as a
	// guard rather than an assumption: emitting a reference to an identifier in
	// another package would produce a file that does not compile, and a
	// generator that can do that has to be stopped by something.
	if target.PkgPath != em.Entity.PkgPath {
		return fmt.Errorf("%s.%s relates to %s in another package, which cannot be generated: the target's descriptors are unexported in its own package; declare them in one package or drop the relation",
			em.Entity.Display(), name, target.Display())
	}

	// The columns are listed once and shared by the folded and batched paths,
	// so the statement and the scanner cannot disagree about their order.
	b.line("// %s are the columns of %s that %s.%s reads.", relColumnsName(entity, name), rel.Target.Table.Qualified(), entity, name)
	b.line("var %s = []string{", relColumnsName(entity, name))
	for _, cm := range rel.Target.Cols {
		b.line("\t%s,", quote(cm.Column.Name))
	}
	b.line("}")
	b.blank()

	b.line("// %s pairs the columns %s matches on, in the order %s declares them.", relKeysName(entity, name), name, rel.FK.Name)
	b.line("var %s = []orm.RelKey{", relKeysName(entity, name))
	for i := range rel.KeyCols {
		b.line("\t{Parent: %s, Target: %s, Type: %s},",
			quote(rel.KeyCols[i].Column.Name),
			quote(rel.TargetCols[i].Column.Name),
			quote(rel.KeyCols[i].Column.Type.String()))
	}
	b.line("}")
	b.blank()

	if rel.Cardinality == model.CardOne {
		c.foldedRelation(b, em, rel)
	}
	c.relationAttach(b, em, rel)
	c.relationRefs(b, em, rel)
	c.relationKeys(b, em, rel)
	return c.relationAux(b, em, rel)
}

// relationRefs renders the reader that hands a loaded relation's rows to the
// relations requested of them.
//
// The pointers go into the caller's slice rather than a fresh one, so flattening
// a level costs one growing slice for the level rather than one allocation per
// parent. They point into the relation the entity now holds, whose storage is
// final: the loader finished attaching before anything asked for these.
func (c *pkgContext) relationRefs(b *builder, em *model.EntityMapping, rel model.RelMapping) {
	entity := em.Entity.Name
	target := rel.Target.Entity.Name
	name := rel.Field.Name

	b.line("// %s appends pointers to the rows %s.%s loaded, which is what a", relRefsName(entity, name), entity, name)
	b.line("// relation of %s is loaded against.", target)
	b.line("func %s(e *%s, out []*%s) []*%s {", relRefsName(entity, name), entity, target, target)
	if rel.Cardinality == model.CardOne {
		b.line("	if v, ok := e.%s.Get(); ok && v != nil {", name)
		b.line("		out = append(out, v)")
		b.line("	}")
		b.line("	return out")
		b.line("}")
		b.blank()
		return
	}
	b.line("	rows, ok := e.%s.Get()", name)
	b.line("	if !ok {")
	b.line("		return out")
	b.line("	}")
	b.line("	for i := range rows {")
	b.line("		out = append(out, &rows[i])")
	b.line("	}")
	b.line("	return out")
	b.line("}")
	b.blank()
}

// relationAux renders the buffer a relation reads its parent keys into when the
// entity does not map them.
//
// The values are decoded with the Go type of the column on the other side of
// the foreign key. That is the same type by definition — a foreign key relates
// two columns PostgreSQL can compare — so nothing here has to invent a mapping
// for a column the author never declared.
func (c *pkgContext) relationAux(b *builder, em *model.EntityMapping, rel model.RelMapping) error {
	if parentKeysMapped(rel) {
		return nil
	}
	entity := em.Entity.Name
	name := rel.Field.Name
	typeName := relAuxTypeName(entity, name)

	types := make([]string, len(rel.KeyCols))
	for i, k := range rel.TargetCols {
		if !k.Mapped() {
			return fmt.Errorf("%s.%s matches %s.%s against %s.%s, and neither is mapped to a Go field, so there is no type to read the key with; map one of them",
				em.Entity.Display(), name,
				em.Table.Qualified(), rel.KeyCols[i].Column.Name,
				rel.Target.Table.Qualified(), k.Column.Name)
		}
		field := rel.Target.Cols[k.FieldIdx].Field
		types[i] = mirrorType(field.Type)
		b.imports(field.Type.SrcRefs...)
	}

	b.line("// %s are the columns %s.%s matches on that %s does not map.", relAuxColumnsName(entity, name), entity, name, entity)
	b.line("//")
	b.line("// The statement that loads the %s selects them as extra values, so that", entity)
	b.line("// this relation has keys to load by without %s having to carry a field it", entity)
	b.line("// was never written with.")
	b.line("var %s = []string{", relAuxColumnsName(entity, name))
	for _, k := range rel.KeyCols {
		b.line("	%s,", quote(k.Column.Name))
	}
	b.line("}")
	b.blank()

	b.line("// %s buffers those values for as long as %s.%s needs them.", typeName, entity, name)
	b.line("//")
	b.line("// A key may be NULL, which is a row with no related row rather than a fault,")
	b.line("// so every value is held as a pointer and travels to PostgreSQL as one.")
	b.line("type %s struct {", typeName)
	for i, t := range types {
		b.line("	k%d []%s", i, t)
	}
	b.line("}")
	b.blank()

	b.line("func %s() orm.AuxKeys { return &%s{} }", relAuxCtorName(entity, name), typeName)
	b.blank()

	b.line("// Next makes room for one more row and returns where to scan it.")
	b.line("func (a *%s) Next() []any {", typeName)
	for i := range types {
		b.line("	a.k%d = append(a.k%d, nil)", i, i)
	}
	b.line("	return []any{")
	for i := range types {
		b.line("		&a.k%d[len(a.k%d)-1],", i, i)
	}
	b.line("	}")
	b.line("}")
	b.blank()

	b.line("// Reorder puts the values in the order the rows were attached in.")
	b.line("func (a *%s) Reorder(order []int) {", typeName)
	for i, t := range types {
		b.line("	k%d := make([]%s, len(order))", i, t)
	}
	b.line("	for i, j := range order {")
	for i := range types {
		b.line("		k%d[i] = a.k%d[j]", i, i)
	}
	b.line("	}")
	for i := range types {
		b.line("	a.k%d = k%d", i, i)
	}
	b.line("}")
	b.blank()

	b.line("// Arrays returns one array per key column.")
	b.line("func (a *%s) Arrays() []any {", typeName)
	b.line("	return []any{")
	for i := range types {
		b.line("		a.k%d,", i)
	}
	b.line("	}")
	b.line("}")
	b.blank()
	return nil
}

// foldedRelation renders the per-row binding of a to-one relation.
func (c *pkgContext) foldedRelation(b *builder, em *model.EntityMapping, rel model.RelMapping) {
	entity := em.Entity.Name
	target := rel.Target.Entity.Name
	name := rel.Field.Name

	b.line("// %s allocates one row's worth of storage for %s.%s and returns the", relBindName(entity, name), entity, name)
	b.line("// function that attaches whatever was scanned.")
	b.line("//")
	b.line("// A relation that was asked for is always set: to the row if there was one,")
	b.line("// and to absent if there was not. Neither is the same as never having asked.")
	b.line("func %s() ([]any, func(*%s)) {", relBindName(entity, name), entity)
	b.line("\tvar n %s", nullsTypeName(target))
	b.line("\treturn %s(&n), func(e *%s) {", nullsDestName(target), entity)
	b.line("\t\tif v, ok := %s(&n); ok {", nullsRestoreName(target))
	b.line("\t\t\te.%s = orm.NewOne(&v)", name)
	b.line("\t\t\treturn")
	b.line("\t\t}")
	b.line("\t\te.%s = orm.Absent[%s]()", name, target)
	b.line("\t}")
	b.line("}")
	b.blank()
}

// relationAttach renders the function that sets a loaded relation on a parent.
func (c *pkgContext) relationAttach(b *builder, em *model.EntityMapping, rel model.RelMapping) {
	entity := em.Entity.Name
	target := rel.Target.Entity.Name
	name := rel.Field.Name

	b.line("// %s sets %s.%s from what a relation statement returned.", relAttachName(entity, name), entity, name)
	b.line("//")
	b.line("// It is called for every parent, including the ones nothing matched: a")
	b.line("// relation that was asked for and found nothing is loaded and empty, which is")
	b.line("// not the same fact as never having asked.")
	if rel.Cardinality == model.CardOne {
		b.line("func %s(e *%s, t *%s) {", relAttachName(entity, name), entity, target)
		b.line("\tif t == nil {")
		b.line("\t\te.%s = orm.Absent[%s]()", name, target)
		b.line("\t\treturn")
		b.line("\t}")
		b.line("\te.%s = orm.NewOne(t)", name)
		b.line("}")
		b.blank()
		return
	}
	b.line("func %s(e *%s, rows []%s) { e.%s = orm.NewManyFrom(rows) }", relAttachName(entity, name), entity, target, name)
	b.blank()
}

// relationKeys renders the reader that collects the parent keys of a relation
// loaded in a statement of its own.
//
// A relation whose parent key columns the entity does not map gets none. That
// is legitimate rather than an error: the relation is still loadable through
// the join, where PostgreSQL reads the key from the row instead. Generating a
// reader that cannot work, or refusing the relation outright, would both be
// worse than saying nothing.
func (c *pkgContext) relationKeys(b *builder, em *model.EntityMapping, rel model.RelMapping) {
	entity := em.Entity.Name
	name := rel.Field.Name
	if !parentKeysMapped(rel) {
		return
	}

	b.line("// %sKeys reads the keys of %s.%s from the parents.", relLoadName(entity, name), entity, name)
	b.line("//")
	b.line("// The keys travel as one array per key column rather than one parameter per")
	b.line("// parent, so one statement covers every parent and the number of statements")
	b.line("// does not grow with the number of rows.")
	b.line("func %sKeys(parents []*%s) ([]any, error) {", relLoadName(entity, name), entity)
	for i, k := range rel.KeyCols {
		field := em.Cols[k.FieldIdx].Field
		b.line("\tk%d := make([]%s, len(parents))", i, field.Type.Src)
		b.imports(field.Type.SrcRefs...)
	}
	b.line("\tfor i, p := range parents {")
	for i, k := range rel.KeyCols {
		b.line("\t\tk%d[i] = p.%s", i, em.Cols[k.FieldIdx].Field.Name)
	}
	b.line("\t}")
	b.line("\treturn []any{")
	for i := range rel.KeyCols {
		b.line("\t\tk%d,", i)
	}
	b.line("\t}, nil")
	b.line("}")
	b.blank()
}

// parentKeysMapped reports whether every key column on the declaring entity's
// side has a Go field to read it from.
func parentKeysMapped(rel model.RelMapping) bool {
	for _, k := range rel.KeyCols {
		if !k.Mapped() {
			return false
		}
	}
	return true
}

// mirrorType is the type a column's value takes while it might be absent.
//
// A type that already carries NULL keeps its own shape; anything else gains a
// pointer, because a plain value has nowhere to record that the row was not
// there.
func mirrorType(t model.GoType) string {
	if absorbsNull(t) {
		return t.Src
	}
	return "*" + t.Src
}

// absorbsNull reports whether scanning SQL NULL into the type succeeds.
//
// This is a wider question than model.GoType.Nullable, which asks whether the
// type can *distinguish* NULL from a value. A slice cannot — nil and empty are
// the same array — but it accepts NULL without failing, which is all the mirror
// needs.
func absorbsNull(t model.GoType) bool {
	switch {
	case t.Ptr, t.SQLNull:
		return true
	case t.Kind == model.KindMap, t.Kind == model.KindAny,
		t.Kind == model.KindSlice, t.Kind == model.KindBytes:
		return true
	default:
		return false
	}
}

// restoreExpr reads a field back out of the mirror.
func restoreExpr(f *model.GoField) string {
	if absorbsNull(f.Type) {
		return "n." + f.Name
	}
	return "*n." + f.Name
}

// presenceCheck renders the condition that means "there was no row".
func presenceCheck(em *model.EntityMapping) (string, error) {
	var parts []string
	for _, pk := range em.Table.PK {
		idx := em.ColIdx(pk)
		if idx < 0 {
			return "", fmt.Errorf("%s does not map the primary key column %s, which a relation to it needs to tell an absent row from a present one",
				em.Entity.Display(), pk.Name)
		}
		parts = append(parts, "n."+em.Cols[idx].Field.Name+" == nil")
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("%s has no primary key, so a relation to it cannot tell an absent row from a present one", em.Entity.Display())
	}
	return strings.Join(parts, " || "), nil
}

// relDescriptor renders the constructor for one relation descriptor.
//
// Two occurrences are handed over. The parent is the src the descriptors are
// being built for, so that an aliased table yields relations correlated against
// the alias. The target is the target's own package-level occurrence, because
// that is what the target's descriptors qualify against — and so the only
// occurrence a caller's relation predicates can name.
func relDescriptor(entity string, rel model.RelMapping) string {
	name := rel.Field.Name
	target := rel.Target.Entity.Name

	var b strings.Builder
	kind, spec := "NewManyRel", "ManyRelSpec"
	if rel.Cardinality == model.CardOne {
		kind, spec = "NewOneRel", "OneRelSpec"
	}
	fmt.Fprintf(&b, "orm.%s(orm.%s[%s, %s]{\n", kind, spec, entity, target)
	fmt.Fprintf(&b, "Name: %s,\n", quote(name))
	// The parent is the occurrence these descriptors are being built for, so an
	// aliased table yields relations correlated against the alias. The target is
	// the target's own occurrence, because that is the one a caller's relation
	// predicates name.
	fmt.Fprintf(&b, "Parent: src,\nTarget: %s,\n", sourceVarName(rel.Target.Table.Name))
	fmt.Fprintf(&b, "Keys: %s,\n", relKeysName(entity, name))
	fmt.Fprintf(&b, "Columns: %s,\n", relColumnsName(entity, name))
	if rel.Cardinality == model.CardOne {
		fmt.Fprintf(&b, "Bind: %s,\n", relBindName(entity, name))
	}
	// A relation whose parent keys are unmapped reads them from the statement
	// that loaded its parents instead, so it brings a buffer rather than a
	// reader.
	if parentKeysMapped(rel) {
		fmt.Fprintf(&b, "ExtractKeys: %sKeys,\n", relLoadName(entity, name))
	} else {
		fmt.Fprintf(&b, "AuxColumns: %s,\n", relAuxColumnsName(entity, name))
		fmt.Fprintf(&b, "NewAux: %s,\n", relAuxCtorName(entity, name))
	}
	fmt.Fprintf(&b, "Dest: %s,\n", relDestName(target))
	fmt.Fprintf(&b, "Attach: %s,\n", relAttachName(entity, name))
	fmt.Fprintf(&b, "Refs: %s,\n", relRefsName(entity, name))
	b.WriteString("})")
	return b.String()
}
