package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The committed manifests describe the API this repository currently has.
//
// This is the gate. It fails when the public surface changes without the
// snapshot being updated in the same commit, which makes every change to the
// contract something a reviewer sees as a diff in api/ rather than something
// they would have to notice by reading the whole pull request.
//
// Updating the snapshot is not the same as approving the change. It is how the
// change gets stated; docs/compatibility.md says which changes are allowed
// under which version bump.
func TestCommittedManifests_areCurrent(t *testing.T) {
	root := repoRoot(t)
	for _, c := range []struct {
		module   string
		manifest string
	}{
		{".", "api/orm.txt"},
		{"./ormotel", "api/ormotel.txt"},
		{"./ormtest/postgres", "api/ormtest-postgres.txt"},
	} {
		t.Run(c.manifest, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(root, c.manifest))
			if err != nil {
				t.Fatalf("reading the committed manifest: %v\n"+
					"regenerate it with: go run ./internal/tools/apimanifest -dir %s -out %s ./...",
					err, c.module, c.manifest)
			}
			got := generate(t, c.module)
			if got == string(want) {
				return
			}
			t.Errorf("the public API of %s differs from %s.\n\n"+
				"If the change is intended, regenerate:\n"+
				"    go run ./internal/tools/apimanifest -dir %s -out %s ./...\n"+
				"and say in the changelog which kind of change it is.\n\nFirst differences:\n%s",
				c.module, c.manifest, c.module, c.manifest, firstDiffs(string(want), got, 25))
		})
	}
}

// firstDiffs renders the leading differences, so a failure names what changed
// rather than printing three thousand identical lines.
func firstDiffs(want, got string, limit int) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	var out []string
	for i := range max(len(wl), len(gl)) {
		var a, b string
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			b = gl[i]
		}
		if a != b {
			out = append(out, "  committed: "+a, "  now:       "+b)
			if len(out) >= limit*2 {
				out = append(out, "  ...")
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
