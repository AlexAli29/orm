#!/usr/bin/env bash
# M16.5 compatibility: a small adversarial sample against the compatibility
# evidence itself.
#
# This is not the E/F mutation campaign and does not reopen it. It attacks the
# failure modes compatibility evidence has — a source identity that forgets its
# schema, a discovery pass that reads one package, an ordering pass that runs
# backwards, a matrix that quietly drops a row, a fixture that renders less than
# it claims, an existence gate that accepts nothing — and requires each to be
# caught by a named test.
#
# Every attack is checked to have changed the file, and the two that need it are
# checked to express their defect before being asked whether anything catches
# them: an edit that compiles to the same behaviour is not evidence of a gap. One
# attack in the first draft of this file reversed a function with no callers, and
# reported a hole that did not exist.
#
#   ORM_ROOT   the module root; defaults to this file's repository
# Environment for the servers is whatever the test suite already needs.
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
attack() { # attack <name> <file> <anchor> <replacement> <run> [<expression-probe>]
  local name="$1" file="$2" old="$3" new="$4" run="$5" probe="${6:-}"
  if ! "$MUTATE" edit -file "$ROOT/$file" -old "$old" -new "$new" >/dev/null 2>&1; then
    echo "  BROKEN   $name (the edit did not apply as declared)"; missed=$((missed+1)); return
  fi
  if [ -n "$probe" ] && go test -count=1 ./cmd/orm -run "$probe" >/dev/null 2>&1; then
    echo "  UNEXPRESSED $name (the probe stayed green, so the edit is not the defect)"
    missed=$((missed+1)); git checkout -- "$file"; return
  fi
  if go test -count=1 ./cmd/orm -run "$run" >/dev/null 2>&1; then
    echo "  MISSED   $name"; missed=$((missed+1))
  else
    echo "  CAUGHT   $name"; caught=$((caught+1))
  fi
  git checkout -- "$file"
}

attack "source identity loses its schema" internal/gen/schema/view.go \
 $'func (m MaterializedView) Qualified() string { return m.Schema + "." + m.Name }' \
 $'func (m MaterializedView) Qualified() string { return m.Name }' \
 '^TestCompatPkg_sameBasenameAcrossSchemasStaysDistinct$'

attack "discovery reads only the first package" internal/gen/pipeline.go \
 $'\ttargets := make([]goscan.Target, 0, len(cfg.Packages))\n\tfor _, p := range cfg.Packages {' \
 $'\ttargets := make([]goscan.Target, 0, len(cfg.Packages))\n\tfor _, p := range cfg.Packages[:1] {' \
 '^TestCompatPkg_materializedViewAcrossPackages$'

attack "relation dependency order reversed" internal/gen/migrate/planrelations.go \
 $'\tfor _, n := range names {\n\t\tif err := visit(n); err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t}\n\treturn out, nil' \
 $'\tfor _, n := range names {\n\t\tif err := visit(n); err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t}\n\tslices.Reverse(out)\n\treturn out, nil' \
 '^TestCompatPkg_materializedViewAcrossPackages$'

attack "search_path loses its second schema" internal/gen/config/config.go \
 $'\t\tcfg.Schema.SearchPath = append(cfg.Schema.SearchPath, exp.string(fmt.Sprintf("schema.search_path[%d]", i), s))' \
 $'\t\tif len(cfg.Schema.SearchPath) == 0 {\n\t\t\tcfg.Schema.SearchPath = append(cfg.Schema.SearchPath, exp.string(fmt.Sprintf("schema.search_path[%d]", i), s))\n\t\t}' \
 '^TestCompatPkg_materializedViewAcrossPackages$'

attack "the major matrix silently drops PG17" cmd/orm/compat_test.go \
 $'\treturn got\n}\n\n// The matrix is five majors' \
 $'\tdelete(got, "PG17")\n\treturn got\n}\n\n// The matrix is five majors' \
 '^TestCompat_theMajorMatrixIsComplete$'

attack "the PostGIS matrix loses a declared combination" cmd/orm/compatgis_test.go \
 $'\t\t{"14", "3.4", "ORM_TEST_DSN_POSTGIS_14_34"},\n' \
 $'' \
 '^TestCompatGIS_theDeclaredMatrixIsComplete$'

attack "the 1000-source fixture renders only 100" cmd/orm/compatscale_test.go \
 $'\tfor i := range n {' \
 $'\tif n > 100 {\n\t\tn = 100\n\t}\n\tfor i := range n {' \
 '^TestCompatScale_projectsOfEverySize$/1000-sources$'

attack "the artifact existence gate returns nothing" cmd/orm/matviewportability_test.go \
 $'func mustRead(t *testing.T, major, what, path string) string {\n\tt.Helper()' \
 $'func mustRead(t *testing.T, major, what, path string) string {\n\tt.Helper()\n\tif true {\n\t\treturn ""\n\t}' \
 '^TestCompat_portableArtifactsAcrossEveryMajor$'

echo
echo "caught: $caught   missed: $missed"
git status --porcelain | sed 's/^/  DIRT: /'
[ "$missed" -eq 0 ]
