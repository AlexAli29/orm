package migrate_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 G1: does the planner scale, and is what it produces correct at scale?
//
// The measurement is secondary. What matters is that a thousand-node plan is
// still topologically valid and still deterministic — a planner that got fast
// by ordering wrongly would be worse than a slow one.

// chainDAG builds n views where each reads the one before it, plus a fan-out
// every tenth node so the graph is not a single line.
func chainDAG(n int) *schema.Schema {
	s := &schema.Schema{}
	for i := range n {
		v := schema.View{
			Schema: "public", Name: fmt.Sprintf("v%04d", i),
			Columns:    []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}}},
			Definition: schema.Definition{SQL: fmt.Sprintf("SELECT id FROM base WHERE id > %d", i)},
		}
		if i > 0 {
			v.DependsOn = append(v.DependsOn,
				schema.RelationRef{Schema: "public", Name: fmt.Sprintf("v%04d", i-1)})
		}
		if i >= 10 {
			v.DependsOn = append(v.DependsOn,
				schema.RelationRef{Schema: "public", Name: fmt.Sprintf("v%04d", i-10)})
		}
		s.Views = append(s.Views, v)
	}
	return s
}

func TestScale_plannerOrdersLargeGraphs(t *testing.T) {
	for _, n := range []int{10, 100, 1000} {
		t.Run(fmt.Sprintf("%d views", n), func(t *testing.T) {
			desired := chainDAG(n)
			edges := 0
			for _, v := range desired.Views {
				edges += len(v.DependsOn)
			}

			start := time.Now()
			d, err := migrate.Compute(&schema.Schema{}, desired, migrate.Options{})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("planning %d views: %v", n, err)
			}
			t.Logf("%d nodes, %d edges, %d operations, %v",
				n, edges, len(d.Operations), elapsed.Round(time.Microsecond))

			if len(d.Operations) != n {
				t.Fatalf("planned %d operations for %d views", len(d.Operations), n)
			}

			// Every view exactly once, and never before something it reads.
			at := make(map[string]int, n)
			for i, op := range d.Operations {
				name := strings.TrimPrefix(op.Describe(), "create view ")
				if _, dup := at[name]; dup {
					t.Fatalf("%s was planned twice", name)
				}
				at[name] = i
			}
			for _, v := range desired.Views {
				for _, dep := range v.DependsOn {
					if at[dep.Qualified()] > at[v.Qualified()] {
						t.Fatalf("%s is created before %s, which it reads",
							v.Qualified(), dep.Qualified())
					}
				}
			}

			// And the same plan every time: an order that depended on map
			// iteration would review differently on each generation.
			again, err := migrate.Compute(&schema.Schema{}, chainDAG(n), migrate.Options{})
			if err != nil {
				t.Fatal(err)
			}
			for i := range d.Operations {
				if d.Operations[i].Describe() != again.Operations[i].Describe() {
					t.Fatalf("two runs produced different orders at position %d", i)
				}
			}
		})
	}
}
