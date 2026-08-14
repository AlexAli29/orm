package migrate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Materialized view operations.
//
// There are two of them, and the absence of a third is the design.
//
// PostgreSQL has no CREATE OR REPLACE MATERIALIZED VIEW. The only way to change
// what one selects is to drop it and create it again, and that is not a
// refinement of replacement — it destroys the stored rows, every index built on
// them, every grant, and every dependent object, and it does so silently. So
// there is no ReplaceMaterializedView here, and the planner refuses a definition
// change rather than assembling one out of these two. A caller who genuinely
// wants the recreation writes it, which is a decision somebody makes once and
// reviews, rather than one a planner infers.

// CreateMaterializedView creates a materialized view.
type CreateMaterializedView struct {
	View schema.MaterializedView
}

func (o CreateMaterializedView) Describe() string {
	if !o.View.WithData {
		return "create materialized view " + o.View.Qualified() + " (with no data)"
	}
	return "create materialized view " + o.View.Qualified()
}

func (o CreateMaterializedView) Safety() Safety      { return Safe }
func (o CreateMaterializedView) Transactional() bool { return true }

func (o CreateMaterializedView) SQL() ([]string, error) {
	return createMaterializedViewSQL(o.View)
}

func (o CreateMaterializedView) Apply(s *schema.Schema) error {
	if _, ok := s.Relation(o.View.Schema, o.View.Name); ok {
		return fmt.Errorf("%s already exists in the migration state", o.View.Qualified())
	}
	m := o.View.Clone()
	// Population is what the server holds at a moment, not what the schema
	// says, and the migration state is schema. Recording it here would make the
	// state disagree with itself the first time anybody refreshed.
	m.Populated = false
	// Indexes are not this operation's to create, and are dropped here rather
	// than only being stripped by the planner.
	//
	// The statement this operation renders creates the relation and nothing
	// else; every index comes from a CreateIndex beside it. So an artifact
	// whose payload carries indexes describes a database that was never built
	// that way, and replaying it would add each index twice — once from here
	// and once from the operation that actually created it. The duplicate is
	// invisible in the database and fatal in the migration state, where the
	// next plan finds an index the declarations do not have and writes a
	// migration to drop it, forever.
	//
	// The planner already strips them. This is for the artifacts it did not
	// write: one committed before that fix, or one edited by hand.
	m.Indexes = nil
	s.MaterializedViews = append(s.MaterializedViews, m)
	s.Normalize()
	return nil
}

func (o CreateMaterializedView) Reverse(*schema.Schema) (Operation, error) {
	return DropMaterializedView{Schema: o.View.Schema, Name: o.View.Name}, nil
}

// DropMaterializedView removes a materialized view.
type DropMaterializedView struct {
	Schema string
	Name   string
}

func (o DropMaterializedView) Describe() string {
	return "drop materialized view " + o.Schema + "." + o.Name
}

// Safety reports Destructive, and means it more than most.
//
// Dropping a materialized view discards rows that may have taken a long time to
// compute and that nothing else holds: unlike a table, there is no expectation
// that the data exists anywhere upstream in the same shape.
func (o DropMaterializedView) Safety() Safety      { return Destructive }
func (o DropMaterializedView) Transactional() bool { return true }

func (o DropMaterializedView) SQL() ([]string, error) {
	name, err := qualifiedIdent(o.Schema, o.Name)
	if err != nil {
		return nil, err
	}
	// RESTRICT is PostgreSQL's default and is written out anyway, for the same
	// reason DropView writes it: a reader of this migration should be able to
	// see that CASCADE was not chosen, rather than having to remember what the
	// default is.
	return []string{"DROP MATERIALIZED VIEW " + name + " RESTRICT"}, nil
}

func (o DropMaterializedView) Apply(s *schema.Schema) error {
	for i, m := range s.MaterializedViews {
		if m.Schema == o.Schema && m.Name == o.Name {
			s.MaterializedViews = slices.Delete(s.MaterializedViews, i, i+1)
			return nil
		}
	}
	return fmt.Errorf("materialized view %s.%s is not in the migration state", o.Schema, o.Name)
}

func (o DropMaterializedView) Reverse(s *schema.Schema) (Operation, error) {
	for _, m := range s.MaterializedViews {
		if m.Schema == o.Schema && m.Name == o.Name {
			return CreateMaterializedView{View: m.Clone()}, nil
		}
	}
	return nil, fmt.Errorf("cannot reverse dropping %s.%s: the migration state has no definition "+
		"to recreate it from", o.Schema, o.Name)
}

// createMaterializedViewSQL renders CREATE MATERIALIZED VIEW.
//
// The identifier goes through the writer that owns quoting; the body does not.
// That asymmetry is the same trust boundary CREATE VIEW has: the definition is
// developer-authored schema SQL, and quoting it would turn a SELECT into a
// string literal.
//
// WITH DATA or WITH NO DATA is always written. PostgreSQL's default is WITH
// DATA, and relying on a default here would mean a migration whose meaning
// depends on knowledge the reader has to bring — while the difference decides
// whether the relation is readable at all when the migration finishes.
func createMaterializedViewSQL(m schema.MaterializedView) ([]string, error) {
	name, err := qualifiedIdent(m.Schema, m.Name)
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(m.Definition.SQL)
	if body == "" {
		return nil, fmt.Errorf("%s has no definition, so there is nothing to create it from",
			m.Qualified())
	}
	data := "WITH DATA"
	if !m.WithData {
		data = "WITH NO DATA"
	}
	return []string{"CREATE MATERIALIZED VIEW " + name + " AS\n" + body + "\n" + data}, nil
}

// Index operations reach materialized views through here.
//
// An index on a materialized view is an ordinary index: the same model, the
// same CREATE INDEX ... ON, the same everything. What differs is only where the
// migration state keeps the relation it belongs to, so the lookup has to
// consider both — and a reduced materialized-view index type, which is what the
// alternative would have been, would have meant a second renderer that could
// silently lose a partial predicate or an opclass.

// indexHolder is a relation in the migration state that can carry indexes.
//
// It is a pointer into the state's own slice, so appending through it mutates
// the state rather than a copy.
type indexHolder struct {
	indexes   *[]schema.Index
	normalize func()
}

// indexHolderOf finds the table or materialized view an index belongs to.
//
// Tables are searched first because they are overwhelmingly the common case,
// and a name can only be one of the two: PostgreSQL has a single namespace for
// relations.
func indexHolderOf(s *schema.Schema, schemaName, name string) (indexHolder, error) {
	for i := range s.Tables {
		if s.Tables[i].Schema == schemaName && s.Tables[i].Name == name {
			t := &s.Tables[i]
			return indexHolder{indexes: &t.Indexes, normalize: t.Normalize}, nil
		}
	}
	for i := range s.MaterializedViews {
		if s.MaterializedViews[i].Schema == schemaName && s.MaterializedViews[i].Name == name {
			m := &s.MaterializedViews[i]
			return indexHolder{indexes: &m.Indexes, normalize: func() {}}, nil
		}
	}
	return indexHolder{}, fmt.Errorf("no table or materialized view %s.%s is in the migration state",
		schemaName, name)
}

// indexesIn returns the indexes a relation has in a state, for reversal.
func indexesIn(s *schema.Schema, schemaName, name string) ([]schema.Index, bool) {
	if t, ok := tableIn(s, schemaName, name); ok {
		return t.Indexes, true
	}
	for _, m := range s.MaterializedViews {
		if m.Schema == schemaName && m.Name == name {
			return m.Indexes, true
		}
	}
	return nil, false
}
