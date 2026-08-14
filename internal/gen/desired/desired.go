// Package desired builds the canonical schema a project's Go declarations ask
// for.
//
// It is the managed-mode counterpart of introspection. Introspection answers
// "what does the database have"; this answers "what do the models want", and
// both answer in the same canonical vocabulary so that the difference between
// them is a diff rather than a translation.
//
// Nothing here connects to PostgreSQL. That is the property the whole managed
// workflow rests on: a migration is computed from what the models say and what
// the migrations already said, so two developers on one branch get the same
// migration whatever database they happen to have running.
//
// It also runs before any code has been generated. A schema declaration names
// Go fields, which the scanner has already resolved, rather than the descriptors
// generation produces — otherwise a fresh project could never create the schema
// its generated code would need to exist first.
package desired

import (
	"cmp"
	"fmt"
	"github.com/AlexAli29/orm/gen/diag"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Input is what building a desired schema needs.
type Input struct {
	Config *config.Config
	// Entities are the scanned entities, in scan order.
	Entities []*model.GoEntity
	// Decls are the schema declarations written on types that are not
	// entities, such as an enum on the Go type that uses it.
	Decls []model.SchemaDecl
}

// Build produces the canonical schema the declarations describe.
//
// Every problem it can find is reported together rather than one at a time: an
// author fixing a schema wants the list, and stopping at the first one turns a
// single pass into several.
func Build(in Input) (*schema.Schema, error) {
	b := &builder{cfg: in.Config, entities: in.Entities, decls: in.Decls, enumsByGoType: map[string]string{}}
	s := b.build()
	if len(b.errs) > 0 {
		return nil, &Error{Problems: b.errs}
	}
	return s, nil
}

// Error reports everything wrong with a set of declarations.
type Error struct{ Problems []string }

func (e *Error) Error() string {
	return "the schema declarations are not usable:\n    " + strings.Join(e.Problems, "\n    ")
}

type builder struct {
	cfg      *config.Config
	entities []*model.GoEntity
	decls    []model.SchemaDecl
	// enumsByGoType maps a Go type name to the PostgreSQL enum declared for it,
	// which is what keeps a named string type from becoming plain text.
	enumsByGoType map[string]string
	errs          []string
}

func (b *builder) fail(format string, args ...any) {
	b.errs = append(b.errs, fmt.Sprintf(format, args...))
}

// failCode records a failure under a registered diagnostic code.
//
// This package returns errors rather than a diag.Report, because it runs before
// there is a database to reconcile against and its failures stop the build
// outright. The code still belongs in the message: it is registered in gen/diag
// like every other, so a project can search for it, suppress on it, or find the
// same finding reported by orm check under the same name.
func (b *builder) failCode(code diag.Code, format string, args ...any) {
	b.errs = append(b.errs, string(code)+": "+fmt.Sprintf(format, args...))
}

func (b *builder) build() *schema.Schema {
	s := &schema.Schema{}

	// Enums and extensions are declared on entities but belong to the schema,
	// so they are collected first and deduplicated: two entities using one enum
	// declare it once between them.
	b.collectTypes(s)

	// One schema-qualified name is one relation, of one kind.
	//
	// PostgreSQL has a single namespace for tables, views and materialized
	// views, so two declarations of one name are always a mistake — and which
	// mistake decides how bad it is. Two entities describing the same table
	// each contribute a Table, and the differ then compares the database
	// against whichever it reached: a second, partial description is read as an
	// instruction to drop every column it does not mention. That is a migration
	// destroying data because of a mistake in Go, and it is the outcome this
	// package exists to make impossible.
	//
	// Two declarations of different kinds are worse, not better. Letting the
	// last one win would mean planning a CREATE VIEW over a name a table
	// occupies, or an ALTER TABLE against a stored query. So the check is over
	// one map for every kind rather than one map per kind: it is the same
	// structural rule the duplicate-table refusal established, generalised to
	// the namespace PostgreSQL actually has.
	//
	// Reconciliation reports the table case as E017, but reconciliation
	// compares Go against a database and does not run on this path: in managed
	// mode the declarations are the schema, so the check has to be here too.
	type claim struct {
		entity *model.GoEntity
		kind   model.RelationKind
	}
	claimed := make(map[string]claim, len(b.entities))
	claim1 := func(name string, e *model.GoEntity) bool {
		prev, dup := claimed[name]
		if !dup {
			claimed[name] = claim{entity: e, kind: e.Kind}
			return true
		}
		if prev.kind != e.Kind {
			b.failCode(diag.E026, "%s: %s declares %s as a %s and %s declares it as a %s; "+
				"PostgreSQL has one namespace for tables, views and materialized views, so one "+
				"of these would be planned over the other",
				e.Pos, prev.entity.Display(), name, prev.kind, e.Display(), e.Kind)
			return false
		}
		b.fail("%s: %s and %s both describe %s %s; one relation has one set of columns, and "+
			"building a schema from both would mean dropping whatever the other one leaves out",
			e.Pos, prev.entity.Display(), e.Display(), e.Kind, name)
		return false
	}

	for _, e := range b.entities {
		switch e.Kind {
		case model.RelTable:
			t, ok := b.table(e)
			if !ok {
				continue
			}
			if !claim1(t.Qualified(), e) {
				continue
			}
			s.Tables = append(s.Tables, t)

		case model.RelView:
			v, ok := b.view(e)
			if !ok {
				continue
			}
			if !claim1(v.Qualified(), e) {
				continue
			}
			s.Views = append(s.Views, v)

		case model.RelMaterializedView:
			m, ok := b.materializedView(e)
			if !ok {
				continue
			}
			if !claim1(m.Qualified(), e) {
				continue
			}
			s.MaterializedViews = append(s.MaterializedViews, m)
		}
	}
	b.checkDependencies(s)
	b.resolveForeignKeys(s)
	s.Normalize()
	return s
}

// collectTypes gathers the enums and extensions declared across the entities.
func (b *builder) collectTypes(s *schema.Schema) {
	seenEnum := make(map[string]model.SchemaDecl)
	seenExt := make(map[string]bool)

	// A package-level declaration comes first, since an enum declared on the Go
	// type that uses it is what binds the two together.
	all := slices.Clone(b.decls)
	for _, e := range b.entities {
		all = append(all, e.Decls...)
	}
	{
		for _, d := range all {
			switch d.Kind {
			case model.DeclEnum:
				ref, err := model.ParseTableRef(d.Name)
				if err != nil {
					b.fail("%s: enum %s: %v", d.Pos, d.Name, err)
					continue
				}
				qualified := b.qualify(ref.Schema) + "." + ref.Name
				if prev, ok := seenEnum[qualified]; ok {
					// The same enum declared twice is only a problem when the
					// two disagree; one type used by two entities is ordinary.
					if !slices.Equal(prev.Labels, d.Labels) {
						b.fail("%s: enum %s is declared twice with different labels: [%s] and [%s]",
							d.Pos, qualified, strings.Join(prev.Labels, " "), strings.Join(d.Labels, " "))
					}
					continue
				}
				seenEnum[qualified] = d
				if d.GoType != "" {
					b.enumsByGoType[d.GoType] = qualified
				}
				s.Enums = append(s.Enums, schema.Enum{
					Schema: b.qualify(ref.Schema), Name: ref.Name, Labels: slices.Clone(d.Labels),
				})
			case model.DeclExtension:
				if seenExt[d.Name] {
					continue
				}
				seenExt[d.Name] = true
				s.Extensions = append(s.Extensions, schema.Extension{Name: d.Name})
			}
		}
	}
}

// qualify resolves an unqualified schema name against the search path's first
// entry, which is where an unqualified object is created.
func (b *builder) qualify(schemaName string) string {
	if schemaName != "" {
		return schemaName
	}
	if b.cfg != nil && len(b.cfg.Schema.SearchPath) > 0 {
		return b.cfg.Schema.SearchPath[0]
	}
	return "public"
}

// table builds one entity's table.
func (b *builder) table(e *model.GoEntity) (schema.Table, bool) {
	t := schema.Table{Schema: b.qualify(e.Table.Schema), Name: e.Table.Name}

	// Field order is the table's column order, which is the contract between
	// the select list and the scanner.
	var pk []string
	seen := make(map[string]model.GoField)
	for _, f := range e.Fields {
		if f.Tags.Ignore || f.IsRelation() {
			continue
		}
		col, ok := b.column(e, f)
		if !ok {
			continue
		}
		if prev, dup := seen[col.Name]; dup {
			b.fail("%s: %s.%s and %s both map to column %s",
				e.Pos, e.Name, prev.Name, f.Name, col.Name)
			continue
		}
		seen[col.Name] = f
		t.Columns = append(t.Columns, col)

		if f.Tags.PK {
			pk = append(pk, col.Name)
		}
		if f.Tags.Unique {
			t.Uniques = append(t.Uniques, schema.Unique{
				Name:       constraintName(t.Name, []string{col.Name}, "key"),
				Columns:    []string{col.Name},
				Constraint: true,
			})
		}
	}
	if len(t.Columns) == 0 {
		b.fail("%s: %s declares no columns", e.Pos, e.Name)
		return schema.Table{}, false
	}
	if len(pk) > 0 {
		t.PrimaryKey = &schema.PrimaryKey{Name: constraintName(t.Name, nil, "pkey"), Columns: pk}
	}

	b.applyDecls(e, &t, seen)
	return t, true
}

// column builds one column from a field and its tags.
func (b *builder) column(e *model.GoEntity, f model.GoField) (schema.Column, bool) {
	name := f.Tags.Column
	if name == "" {
		name = snake(f.Name)
	}

	typ, nullable, err := b.columnType(f)
	if err != nil {
		b.fail("%s: %s.%s: %v", e.Pos, e.Name, f.Name, err)
		return schema.Column{}, false
	}

	c := schema.Column{Name: name, Type: typ, Nullable: nullable}
	if f.Tags.HasIdentity {
		switch f.Tags.Identity {
		case model.IdentityAlways:
			c.Identity = schema.IdentityAlways
		default:
			c.Identity = schema.IdentityByDefault
		}
		// An identity column supplies its own value, so it is never nullable
		// and never has a default of its own.
		c.Nullable = false
	}
	if f.Tags.HasDefault {
		c.Default = schema.Expr(f.Tags.Default)
	}
	if f.Tags.Generated != "" {
		c.Generated = schema.Expr(f.Tags.Generated)
	}

	switch {
	case c.Identity != schema.NotIdentity && !c.Default.Empty():
		b.fail("%s: %s.%s is an identity column and also declares a default; an identity column supplies its own value",
			e.Pos, e.Name, f.Name)
	case !c.Generated.Empty() && !c.Default.Empty():
		b.fail("%s: %s.%s is a generated column and also declares a default; a generated column computes its own value",
			e.Pos, e.Name, f.Name)
	case !c.Generated.Empty() && c.Identity != schema.NotIdentity:
		b.fail("%s: %s.%s is both generated and an identity column", e.Pos, e.Name, f.Name)
	}
	return c, true
}

// applyDecls enriches a table with its struct-level declarations.
//
// The rule is that a field tag establishes and a declaration enriches. A
// declaration naming an object a tag already created is a conflict rather than
// an override, because two places saying different things about one object is
// a question nobody can answer from the outside.
func (b *builder) applyDecls(e *model.GoEntity, t *schema.Table, fields map[string]model.GoField) {
	byField := make(map[string]string, len(e.Fields))
	for _, f := range e.Fields {
		if f.Tags.Ignore || f.IsRelation() {
			continue
		}
		name := f.Tags.Column
		if name == "" {
			name = snake(f.Name)
		}
		byField[f.Name] = name
	}
	// A declaration may name either the Go field or the column; the field is
	// the documented form, and accepting the column too costs nothing and
	// removes a papercut.
	resolve := func(d model.SchemaDecl, ref string) (string, bool) {
		if col, ok := byField[ref]; ok {
			return col, true
		}
		if _, ok := fields[ref]; ok {
			return ref, true
		}
		b.fail("%s: %s %s refers to %q, which %s has no field or column for",
			d.Pos, d.Kind, d.Name, ref, e.Name)
		return "", false
	}

	for _, d := range e.Decls {
		switch d.Kind {
		case model.DeclEnum, model.DeclExtension:
			continue

		case model.DeclCheck:
			if strings.TrimSpace(d.Expr) == "" {
				b.fail("%s: check %s has no condition", d.Pos, d.Name)
				continue
			}
			if slices.ContainsFunc(t.Checks, func(c schema.Check) bool { return c.Name == d.Name }) {
				b.fail("%s: check %s is declared more than once on %s", d.Pos, d.Name, e.Name)
				continue
			}
			t.Checks = append(t.Checks, schema.Check{Name: d.Name, Expression: schema.Expr(d.Expr)})

		case model.DeclUnique:
			cols := b.resolveList(d, resolve)
			if cols == nil {
				continue
			}
			if slices.ContainsFunc(t.Uniques, func(u schema.Unique) bool { return u.Name == d.Name }) {
				b.fail("%s: unique %s is declared more than once on %s", d.Pos, d.Name, e.Name)
				continue
			}
			t.Uniques = append(t.Uniques, schema.Unique{Name: d.Name, Columns: cols, Constraint: true})

		case model.DeclIndex:
			idx, ok := b.index(d, resolve)
			if !ok {
				continue
			}
			if slices.ContainsFunc(t.Indexes, func(i schema.Index) bool { return i.Name == idx.Name }) ||
				slices.ContainsFunc(t.Uniques, func(u schema.Unique) bool { return u.Name == idx.Name }) {
				b.fail("%s: %s is declared more than once on %s", d.Pos, idx.Name, e.Name)
				continue
			}
			// A unique index is an index, and lives in exactly one place. It
			// was once also recorded as a uniqueness object, which produced a
			// schema claiming the same object twice — and a CREATE TABLE that
			// built it twice, the second time under a name already taken.
			t.Indexes = append(t.Indexes, idx)
		}
	}
}

func (b *builder) resolveList(d model.SchemaDecl, resolve func(model.SchemaDecl, string) (string, bool)) []string {
	out := make([]string, 0, len(d.Fields))
	for _, f := range d.Fields {
		if f.Expression != "" {
			b.fail("%s: %s %s: an expression cannot be a constraint column", d.Pos, d.Kind, d.Name)
			return nil
		}
		col, ok := resolve(d, f.Field)
		if !ok {
			return nil
		}
		out = append(out, col)
	}
	if dup := firstDuplicate(out); dup != "" {
		b.fail("%s: %s %s names %s twice", d.Pos, d.Kind, d.Name, dup)
		return nil
	}
	return out
}

func (b *builder) index(d model.SchemaDecl, resolve func(model.SchemaDecl, string) (string, bool)) (schema.Index, bool) {
	idx := schema.Index{
		Name:         d.Name,
		Unique:       d.Unique,
		Method:       d.Method,
		Where:        schema.Expr(d.Expr),
		Concurrently: d.Concurrently,
	}
	for _, f := range d.Fields {
		key := schema.IndexColumn{OpClass: f.OpClass}
		switch {
		case f.Expression != "":
			key.Expression = schema.Expr(f.Expression)
		default:
			col, ok := resolve(d, f.Field)
			if !ok {
				return schema.Index{}, false
			}
			key.Name = col
		}
		if f.Desc {
			key.Direction = schema.Desc
		}
		switch {
		case f.NullsFirst:
			key.Nulls = schema.NullsFirst
		case f.NullsLast:
			key.Nulls = schema.NullsLast
		}
		idx.Columns = append(idx.Columns, key)
	}
	for _, ref := range d.Include {
		col, ok := resolve(d, ref)
		if !ok {
			return schema.Index{}, false
		}
		// A covering column is not a key column, and listing one in both places
		// asks PostgreSQL for something it refuses.
		if slices.Contains(idx.ColumnNames(), col) {
			b.fail("%s: index %s includes %s, which is already one of its keys", d.Pos, d.Name, col)
			return schema.Index{}, false
		}
		idx.Include = append(idx.Include, col)
	}
	if dup := firstDuplicate(idx.Include); dup != "" {
		b.fail("%s: index %s includes %s twice", d.Pos, d.Name, dup)
		return schema.Index{}, false
	}
	if len(idx.Columns) == 0 {
		b.fail("%s: index %s has no keys", d.Pos, d.Name)
		return schema.Index{}, false
	}
	return idx, true
}

func firstDuplicate(names []string) string {
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			return n
		}
		seen[n] = true
	}
	return ""
}

// constraintName derives a deterministic name for an object nobody named.
//
// PostgreSQL's own convention is followed — table_column_key,
// table_column_fkey, table_pkey — because a schema adopted from an existing
// database should keep the names it already has rather than acquiring a second
// set. A name too long for an identifier is truncated with a hash of the whole,
// so two long names that share a prefix cannot collide silently.
func constraintName(table string, columns []string, suffix string) string {
	parts := append([]string{table}, columns...)
	parts = append(parts, suffix)
	return truncateIdentifier(strings.Join(parts, "_"))
}

// maxIdentifier is PostgreSQL's NAMEDATALEN - 1. A longer name is silently
// truncated by the server, which is how two distinct names become one.
const maxIdentifier = 63

func truncateIdentifier(name string) string {
	if len(name) <= maxIdentifier {
		return name
	}
	// The hash is of the whole name, so the result is stable and distinct for
	// distinct inputs.
	sum := fnv1a(name)
	suffix := fmt.Sprintf("_%08x", sum)
	return name[:maxIdentifier-len(suffix)] + suffix
}

// fnv1a is a small deterministic hash. It is not a security primitive: what is
// needed is a stable short value, identical on every machine.
func fnv1a(s string) uint32 {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	h := uint32(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return h
}

// sortStrings returns a sorted copy, for the places a set is rendered.
func sortStrings(in []string) []string {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(a, b) })
	return out
}
