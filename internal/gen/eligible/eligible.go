// Package eligible owns one rule: which index lets PostgreSQL refresh a
// materialized view concurrently.
//
// It exists because the rule was written twice. The emitter and the lock read
// it from the introspection model; the canonical schema had its own copy for
// its own index type. The two agreed, and nothing made them agree — a mutation
// campaign found that changing one left every generation and fingerprint test
// green, which is the shape of a rule that can drift apart silently. When it
// did drift, the fingerprint would say nothing had changed while the generated
// descriptor was wrong.
//
// So the rule lives here, in a package with no dependencies, and both models
// pass their own indexes through it. Neither can restate it without adding
// obviously new code in an obviously wrong place.
//
// The rule is PostgreSQL's and is entirely visible in desired metadata: at
// least one unique index covering every row, built from plain column names. A
// partial index constrains only the rows it covers; an expression index has no
// column list to match rows by. Neither is a candidate, and offering one would
// mean generating a descriptor that sends a REFRESH the server was always going
// to reject. When several qualify the lowest name wins, so the answer never
// depends on the order a catalog or a declaration happened to supply.
package eligible

// Candidate is one index, described only by what the rule needs.
type Candidate struct {
	// Name is the index's name; the lowest qualifying one is chosen.
	Name string
	// Unique reports a unique index. Only a unique index can qualify.
	Unique bool
	// Partial reports a WHERE clause, which covers only some rows.
	Partial bool
	// Expression reports a key that is not a plain column.
	Expression bool
	// Columns is how many keys the index has; an index over none qualifies for
	// nothing.
	Columns int
}

// Qualifies reports whether one index meets PostgreSQL's requirement.
func (c Candidate) Qualifies() bool {
	return c.Unique && !c.Partial && !c.Expression && c.Columns > 0
}

// Choose returns the name of the index a concurrent refresh may use, or the
// empty string when none qualifies.
//
// The lowest name wins among candidates, deterministically, so that two runs
// over one schema — and a generated descriptor and a fingerprint computed from
// it — always name the same index.
func Choose(candidates []Candidate) string {
	best := ""
	for _, c := range candidates {
		if !c.Qualifies() {
			continue
		}
		if best == "" || c.Name < best {
			best = c.Name
		}
	}
	return best
}
