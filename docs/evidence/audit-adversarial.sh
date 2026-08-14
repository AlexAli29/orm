#!/usr/bin/env bash
# M16.5 adversarial audit: mutations for the guarantees this audit added.
#
# The frozen G2 campaign is 46 classes and is not re-run here; its manifest is the
# authority for it. What this covers is the structural guarantees the audit itself
# introduced, each of which needs a killer of its own:
#
#   the duplicated-CreateIndex guard at replay
#   unique equality having one authority
#   both callers of unique equality reaching it
#
# Two attacks from the audit brief are recorded as inexpressible rather than
# missed, with the reason, below.
#
#   ORM_ROOT   the module root; defaults to this file's repository
set -uo pipefail
ROOT=${ORM_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
cd "$ROOT" || exit 2
# The harness is the repository's own Go tool, built once. Its anchor guard is
# what makes an attack that no longer matches a BROKEN result rather than a
# silent pass.
MUTATE=$(mktemp -d)/mutate
go build -o "$MUTATE" ./internal/tools/mutate || exit 2

if [ -n "$(git status --porcelain)" ]; then
  echo "the working tree is dirty; these attacks edit and restore files" >&2; exit 2
fi

caught=0; missed=0
attack() { # attack <name> <file> <anchor> <replacement> <pkg> <run>
  local name="$1" file="$2" old="$3" new="$4" pkg="$5" run="$6"
  if ! "$MUTATE" edit -file "$ROOT/$file" -old "$old" -new "$new" >/dev/null 2>&1; then
    echo "  BROKEN   $name (the edit did not apply as declared)"; missed=$((missed+1)); return
  fi
  if go test -count=1 "$pkg" -run "$run" >/dev/null 2>&1; then
    echo "  MISSED   $name"; missed=$((missed+1))
  else
    echo "  CAUGHT   $name"; caught=$((caught+1))
  fi
  git checkout -- "$file"
}

attack "the duplicated-CreateIndex guard is removed" internal/gen/migrate/ops_constraint.go \
 $'\tif slices.ContainsFunc(*h.indexes, func(x schema.Index) bool { return x.Name == o.Index.Name }) {\n\t\treturn fmt.Errorf("index %s is already on %s.%s in the migration state",\n\t\t\to.Index.Name, o.Schema, o.Table)\n\t}\n' \
 $'' \
 ./internal/gen/migrate '^TestAuditArtifact_impossibleSequencesAreRefusedAtReplay$'

attack "unique equality forgets NULLS NOT DISTINCT" internal/gen/schema/equal.go \
 $'\t\tsameExpr(a.Where, b.Where) && a.NullsNotDistinct == b.NullsNotDistinct' \
 $'\t\tsameExpr(a.Where, b.Where)' \
 ./internal/gen/migrate '^TestUniqueComparison_'

attack "unique equality compares columns as a set" internal/gen/schema/equal.go \
 $'\treturn slices.Equal(a.Columns, b.Columns) && a.Constraint == b.Constraint &&' \
 $'\tac, bc := slices.Clone(a.Columns), slices.Clone(b.Columns)\n\tslices.Sort(ac)\n\tslices.Sort(bc)\n\treturn slices.Equal(ac, bc) && a.Constraint == b.Constraint &&' \
 ./internal/gen/migrate '^TestUniqueComparison_'

attack "the planner restates unique equality and ignores the predicate" internal/gen/migrate/diff.go \
 $'func sameUnique(a, b schema.Unique) bool { return schema.SameUnique(a, b) }' \
 $'func sameUnique(a, b schema.Unique) bool {\n\treturn slices.Equal(a.Columns, b.Columns) && a.Constraint == b.Constraint &&\n\t\ta.NullsNotDistinct == b.NullsNotDistinct\n}' \
 ./internal/gen/migrate '^TestUniqueComparison_'

echo
echo "caught: $caught   missed: $missed"
echo
echo "recorded as inexpressible rather than missed:"
echo "  the transaction commits despite a cancelled context — cancellation lands inside an"
echo "    operation, runAtomic returns from the operation loop, and tx.Commit is never"
echo "    reached. Verified by probe: a panic placed at the commit for a cancelled context"
echo "    does not fire in any cancellation case, so the mutated line is unreachable there."
echo "  provenance written outside the transaction — the migration's transaction is begun"
echo "    on the migrator's own connection, so a write issued through that connection lands"
echo "    inside it anyway. The mutation cannot express the defect it aims at."
echo "  history written outside the transaction — the same, for the same reason."
git status --porcelain | sed 's/^/  DIRT: /'
[ "$missed" -eq 0 ]
