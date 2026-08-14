package diagnostics_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/diagnostics"
	"github.com/AlexAli29/orm/plan"
)

// Plan trees that are not the shape anybody meant.
//
// A plan arrives as JSON from a server, and a decoder for somebody else's
// output has to survive shapes nobody would write on purpose. Depth is bounded
// by encoding/json's own guard, which refuses past about ten thousand levels
// before this package's recursion is reached; a thousand levels parse and
// traverse. Width is linear.

// Audit item 36: a very deep plan must not exhaust the stack.
func TestAudit_deepPlan(t *testing.T) {
	for _, depth := range []int{100, 1000, 10000, 100000} {
		var b strings.Builder
		b.WriteString(`[{"Plan":`)
		for range depth {
			b.WriteString(`{"Node Type":"Nested Loop","Plan Rows":1,"Plans":[`)
		}
		b.WriteString(`{"Node Type":"Seq Scan","Plan Rows":1}`)
		for range depth {
			b.WriteString(`]}`)
		}
		b.WriteString(`}]`)

		p, err := plan.Parse([]byte(b.String()))
		if err != nil {
			t.Logf("depth %d: parse refused: %v", depth, err)
			continue
		}
		t.Logf("depth %d: parsed, tree depth %d", depth, p.Depth())
		_ = p.Nodes()
		_ = p.Summarize()
		_ = diagnostics.FromPlan(p)
		_ = p.String()
	}
}

// Audit item 37: a very wide plan must not be quadratic.
func TestAudit_widePlan(t *testing.T) {
	var b strings.Builder
	b.WriteString(`[{"Plan":{"Node Type":"Append","Plan Rows":1,"Plans":[`)
	for i := range 20000 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"Node Type":"Seq Scan","Relation Name":"p","Plan Rows":1}`)
	}
	b.WriteString(`]}}]`)

	p, err := plan.Parse([]byte(b.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n := len(p.Nodes()); n != 20001 {
		t.Errorf("nodes = %d", n)
	}
	_ = p.Summarize()
	_ = diagnostics.FromPlan(p)
}
