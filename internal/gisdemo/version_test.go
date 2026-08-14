package gisdemo_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The M16 audit: the matrix must prove which server it ran against.
//
// A support matrix is a claim about specific versions, and a job cannot support
// that claim unless it checks that it is talking to the version it was
// configured for. An image tag is easy to get wrong — a typo, a moved floating
// tag, a registry serving something else — and the failure is invisible: the
// suite passes against whatever it actually reached, and the matrix reports
// support for a version nothing tested.
//
// So when a job declares what it expects, this asserts it before anything else
// runs. Locally, with nothing declared, it reports what it found and passes.

// The environment variables a matrix job sets to state what it believes it is
// running against.
const (
	envWantPG      = "ORM_EXPECT_POSTGRES_MAJOR"
	envWantPostGIS = "ORM_EXPECT_POSTGIS_VERSION"
)

func TestMatrix_serverIsTheOneTheJobDeclared(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()

	var version string
	var num int
	if err := pool.QueryRow(ctx,
		`select current_setting('server_version'), current_setting('server_version_num')::int`).
		Scan(&version, &num); err != nil {
		t.Fatalf("asking the server its version: %v", err)
	}
	var gis string
	if err := pool.QueryRow(ctx, `select postgis_lib_version()`).Scan(&gis); err != nil {
		t.Fatalf("asking PostGIS its version: %v", err)
	}
	major := num / 10000
	t.Logf("PostgreSQL %s (major %d), PostGIS %s", version, major, gis)

	if want := os.Getenv(envWantPG); want != "" {
		if got := strconv.Itoa(major); got != want {
			t.Errorf("%s=%s but the server is PostgreSQL major %s (%s): "+
				"this job is not testing the version it claims to",
				envWantPG, want, got, version)
		}
	}
	if want := os.Getenv(envWantPostGIS); want != "" {
		// A prefix, because the image pins a minor the matrix does not name:
		// declaring 3.4 must accept 3.4.2 and reject 3.5.0.
		if !strings.HasPrefix(gis, want+".") && gis != want {
			t.Errorf("%s=%s but the server reports PostGIS %s: "+
				"this job is not testing the PostGIS version it claims to",
				envWantPostGIS, want, gis)
		}
	}
}

// The gate that stops a PostGIS job going quietly green.
//
// Every spatial test skips when the extension is missing, which is right on a
// developer's machine and wrong in CI — a job pointed at an image without
// PostGIS skipped all 49 and reported success. These are the four cases.
func TestMatrix_missingPostGISFailsOnlyWhenRequired(t *testing.T) {
	for _, c := range []struct {
		what      string
		available bool
		required  string
		wantMsg   bool
		wantFatal bool
	}{
		{"present, not required", true, "", false, false},
		{"present and required", true, "1", false, false},
		{"absent on a developer's machine", false, "", true, false},
		{"absent in a job that exists to prove it", false, "1", true, true},
	} {
		t.Run(c.what, func(t *testing.T) {
			msg, fatal := absentPostGIS(c.available, c.required)
			if (msg != "") != c.wantMsg || fatal != c.wantFatal {
				t.Errorf("absentPostGIS(%v, %q) = (%q, %v), want message=%v fatal=%v",
					c.available, c.required, msg, fatal, c.wantMsg, c.wantFatal)
			}
		})
	}
}
