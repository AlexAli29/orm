package migrate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// View operations.
//
// A view is its own operation, never a table with a flag. The schema split made
// in M16.5.1 survives all the way here for the same reason it was made: the
// first code path that treated a view as a table would be one emitting ALTER
// TABLE against a stored query, and it would look correct until it ran.

// CreateView creates an ordinary view.
type CreateView struct {
	View schema.View
}

func (o CreateView) Describe() string       { return "create view " + o.View.Qualified() }
func (o CreateView) Safety() Safety         { return Safe }
func (o CreateView) Transactional() bool    { return true }
func (o CreateView) SQL() ([]string, error) { return createViewSQL(o.View, false) }

func (o CreateView) Apply(s *schema.Schema) error {
	if _, ok := s.Relation(o.View.Schema, o.View.Name); ok {
		return fmt.Errorf("%s already exists in the migration state", o.View.Qualified())
	}
	s.Views = append(s.Views, o.View.Clone())
	s.Normalize()
	return nil
}

func (o CreateView) Reverse(*schema.Schema) (Operation, error) {
	return DropView{Schema: o.View.Schema, Name: o.View.Name}, nil
}

// ReplaceView replaces a view's body with CREATE OR REPLACE VIEW.
//
// It is only ever produced when the planner has proved PostgreSQL will accept
// it — see [ReplaceEligible]. Replacement is preferred over a drop and a create
// because PostgreSQL keeps everything that is not the SELECT: the grants, the
// ownership, the dependent objects, the options. A drop and create loses all of
// them, and loses them silently.
type ReplaceView struct {
	View schema.View
}

func (o ReplaceView) Describe() string       { return "replace view " + o.View.Qualified() }
func (o ReplaceView) Safety() Safety         { return Safe }
func (o ReplaceView) Transactional() bool    { return true }
func (o ReplaceView) SQL() ([]string, error) { return createViewSQL(o.View, true) }

func (o ReplaceView) Apply(s *schema.Schema) error {
	for i, v := range s.Views {
		if v.Schema == o.View.Schema && v.Name == o.View.Name {
			s.Views[i] = o.View.Clone()
			s.Normalize()
			return nil
		}
	}
	return fmt.Errorf("%s is not in the migration state, so there is nothing to replace",
		o.View.Qualified())
}

func (o ReplaceView) Reverse(s *schema.Schema) (Operation, error) {
	for _, v := range s.Views {
		if v.Schema == o.View.Schema && v.Name == o.View.Name {
			return ReplaceView{View: v.Clone()}, nil
		}
	}
	return nil, fmt.Errorf("cannot reverse the replacement of %s: the migration state has no "+
		"earlier definition to restore", o.View.Qualified())
}

// DropView drops a view.
//
// Never with CASCADE. PostgreSQL refuses to drop a view something else depends
// on, and that refusal is the feature: it is the database telling the truth
// about a dependency nothing in this project modelled. CASCADE would turn that
// sentence into silent removal of objects nobody listed, which is the one
// outcome a reviewable migration must not be able to produce by default.
type DropView struct {
	Schema string
	Name   string
}

func (o DropView) Describe() string    { return "drop view " + o.Schema + "." + o.Name }
func (o DropView) Safety() Safety      { return Destructive }
func (o DropView) Transactional() bool { return true }

func (o DropView) SQL() ([]string, error) {
	name, err := qualifiedIdent(o.Schema, o.Name)
	if err != nil {
		return nil, err
	}
	// RESTRICT is PostgreSQL's default and is written anyway, because a reader
	// of this migration should be able to see that CASCADE was not chosen
	// rather than having to know what the default is.
	return []string{"DROP VIEW " + name + " RESTRICT"}, nil
}

func (o DropView) Apply(s *schema.Schema) error {
	for i, v := range s.Views {
		if v.Schema == o.Schema && v.Name == o.Name {
			s.Views = slices.Delete(s.Views, i, i+1)
			return nil
		}
	}
	return fmt.Errorf("view %s.%s is not in the migration state", o.Schema, o.Name)
}

func (o DropView) Reverse(s *schema.Schema) (Operation, error) {
	for _, v := range s.Views {
		if v.Schema == o.Schema && v.Name == o.Name {
			return CreateView{View: v.Clone()}, nil
		}
	}
	return nil, fmt.Errorf("cannot reverse dropping %s.%s: the migration state has no definition "+
		"to recreate it from", o.Schema, o.Name)
}

// createViewSQL renders CREATE [OR REPLACE] VIEW.
//
// The identifier goes through the writer that owns quoting. The body does not:
// it is developer-authored schema SQL, and quoting it would turn a SELECT into
// a string literal. That asymmetry is the trust boundary, and it is the same
// one //orm:check has had since M9.
func createViewSQL(v schema.View, replace bool) ([]string, error) {
	name, err := qualifiedIdent(v.Schema, v.Name)
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(v.Definition.SQL)
	if body == "" {
		return nil, fmt.Errorf("%s has no definition, so there is nothing to create it from",
			v.Qualified())
	}
	head := "CREATE VIEW "
	if replace {
		head = "CREATE OR REPLACE VIEW "
	}
	return []string{head + name + " AS\n" + body}, nil
}

// ReplaceRefusal says why a replacement cannot be proved safe, and is empty
// when it can.
type ReplaceRefusal string

// ReplaceEligible reports whether PostgreSQL will accept CREATE OR REPLACE VIEW
// for this transition, and says why not when it will not.
//
// PostgreSQL's rule is exact and narrow: every column the view already outputs
// must keep its position, its name and its type. New columns may be added, but
// only after the existing ones. What produces those columns may change however
// much it likes — a different predicate, a different join, a different
// expression — which is what makes replacement the useful path.
//
// The proof is made here, before any SQL is written. Emitting the statement and
// letting the server decide would mean the migration's safety was discovered
// during a deployment, which is the wrong place to find out.
//
// Two things deliberately play no part.
//
// Type comparison is PostgreSQL's own type identity, not castability and not Go
// convertibility. int4 and int8 are both integers a Go int64 could hold, and
// PostgreSQL will still refuse to replace one output column with the other.
//
// Nullability plays no part at all. PostgreSQL records none on a view column
// and does not consider it when accepting a replacement, so treating it as part
// of the proof would refuse legal migrations over metadata the server never
// looks at. It matters for generated scanning, which is a different question
// asked in a different place.
func ReplaceEligible(actual, desired schema.View) ReplaceRefusal {
	if len(desired.Columns) < len(actual.Columns) {
		return ReplaceRefusal(fmt.Sprintf(
			"%s would lose %d output column(s). PostgreSQL accepts a replacement only when every "+
				"existing output column survives in place; removing one needs an explicit migration "+
				"that says what happens to whatever reads it",
			desired.Qualified(), len(actual.Columns)-len(desired.Columns)))
	}
	for i, a := range actual.Columns {
		d := desired.Columns[i]
		if a.Name != d.Name {
			return ReplaceRefusal(fmt.Sprintf(
				"%s: output column %d is named %q and would become %q. PostgreSQL accepts a "+
					"replacement only when existing output columns keep their names, and a rename "+
					"silently changes what every caller selecting by name receives",
				desired.Qualified(), i+1, a.Name, d.Name))
		}
		if !sameOutputType(a.Type, d.Type) {
			return ReplaceRefusal(fmt.Sprintf(
				"%s: output column %q is %s and would become %s. PostgreSQL accepts a replacement "+
					"only when existing output columns keep their exact type — not a castable one, "+
					"and not one Go would convert",
				desired.Qualified(), a.Name, a.Type, d.Type))
		}
	}
	return ""
}

// sameOutputType compares PostgreSQL type identity.
func sameOutputType(a, b schema.Type) bool { return a.String() == b.String() }

// qualifiedIdent renders schema.name through the writer that owns quoting, so
// a name needing quotes gets them and none is ever concatenated by hand.
func qualifiedIdent(schemaName, name string) (string, error) {
	return qualified(schemaName, name), nil
}
