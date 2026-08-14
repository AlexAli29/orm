package reconcile_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/reconcile"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 G2 #33: the three shapes of materialized-view index drift, one at a time.
//
// An index can disagree with its declaration in three different ways, and they
// are three different findings: the database is missing one the declarations
// have, the database has one the declarations do not, and both have it but they
// differ. The end-to-end drift tests cover all three, but each through its own
// project — so a change that suppressed exactly one of them would turn exactly
// one end-to-end test red, and nothing would say which shape had been lost or
// that the other two still worked.
//
// That distinction is what made the missing-index class hard to express. A
// mutation broad enough to be obvious removes index checking altogether; the one
// worth attacking suppresses the missing case and leaves the other two reporting
// normally, which is the version a reader of the code would not notice. So the
// three shapes are asserted separately, from one baseline, against the
// reconciler directly.

// matViewWith returns a schema holding one materialized view with the given
// indexes.
func matViewWith(indexes ...schema.Index) *schema.Schema {
	return &schema.Schema{MaterializedViews: []schema.MaterializedView{{
		Schema: "public", Name: "totals", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id AS user_id, email FROM users"},
		Columns: []schema.Column{
			{Name: "user_id", Type: schema.Type{Name: "int8"}},
			{Name: "email", Type: schema.Type{Name: "text"}},
		},
		Indexes: indexes,
	}}}
}

func indexOn(name, column string) schema.Index {
	return schema.Index{Name: name, Columns: []schema.IndexColumn{{Name: column}}}
}

// findings runs the reconciler over one declared and one live schema.
func findings(t *testing.T, want, actual *schema.Schema) []string {
	t.Helper()
	report := &diag.Report{}
	reconcile.CheckMaterializedIndexes(report, want, actual)
	var out []string
	for _, f := range report.Findings() {
		if f.Code != diag.E036 {
			t.Errorf("an index difference was reported as %s rather than E036: %s",
				f.Code, f.Message)
		}
		out = append(out, f.Message)
	}
	return out
}

func TestMaterializedIndexes_theThreeShapesAreReportedSeparately(t *testing.T) {
	declared := indexOn("totals_email_idx", "email")
	other := indexOn("totals_user_id_idx", "user_id")
	changed := declared
	changed.Unique = true

	// Agreement is silence. Without this the other three cases could all be
	// satisfied by a reconciler that complains about everything.
	if got := findings(t, matViewWith(declared), matViewWith(declared)); len(got) != 0 {
		t.Errorf("a materialized view whose indexes match its declaration was reported: %v", got)
	}

	for _, c := range []struct {
		shape        string
		want, actual *schema.Schema
		expect       string
	}{
		{
			// The declarations have an index the database does not: somebody
			// dropped it by hand, or a migration was never applied.
			"an index missing from the database",
			matViewWith(declared), matViewWith(),
			"only in the second schema",
		},
		{
			// The database has one the declarations do not.
			"an index the declarations do not have",
			matViewWith(), matViewWith(declared),
			"only in the first schema",
		},
		{
			// Both have it under the same name and they are not the same index.
			"an index that differs",
			matViewWith(changed), matViewWith(declared),
			"differs",
		},
	} {
		t.Run(c.shape, func(t *testing.T) {
			got := findings(t, c.want, c.actual)
			if len(got) != 1 {
				t.Fatalf("%s produced %d findings, want exactly one: %v", c.shape, len(got), got)
			}
			if !strings.Contains(got[0], "totals_email_idx") {
				t.Errorf("the finding does not name the index: %s", got[0])
			}
			if !strings.Contains(got[0], c.expect) {
				t.Errorf("the finding does not state the difference as %q: %s", c.expect, got[0])
			}
			if !strings.Contains(got[0], "public.totals") {
				t.Errorf("the finding does not name the relation: %s", got[0])
			}
		})
	}

	// And the shapes do not mask each other: two at once are both reported.
	got := findings(t, matViewWith(declared, other), matViewWith(changed))
	if len(got) != 2 {
		t.Errorf("a missing index alongside a changed one produced %d findings, want two: %v",
			len(got), got)
	}
}

// A relation the database does not have at all produces no index findings.
//
// The entity tier reports a missing relation, more usefully. Index findings
// about a relation that is not there would be noise about a fact already stated.
func TestMaterializedIndexes_anAbsentRelationIsNotAnIndexFinding(t *testing.T) {
	got := findings(t, matViewWith(indexOn("totals_email_idx", "email")), &schema.Schema{})
	if len(got) != 0 {
		t.Errorf("a materialized view absent from the database produced index findings: %v", got)
	}
}
