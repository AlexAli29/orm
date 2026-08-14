package reconcile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/testdb"
)

// A fixture is a directory under testdata/fixtures holding
//
//	schema.sql     the PostgreSQL schema
//	orm.yaml       the configuration, whose DSN reads ${ORM_FIXTURE_DSN}
//	domain/*.go    the entity structs
//	want.txt       the golden text report
//	want.json      the golden JSON report
//	want.github    the golden GitHub annotations
//
// Run with -update to regenerate the goldens.
var update = flagUpdate()

func flagUpdate() bool { return os.Getenv("ORM_UPDATE_GOLDEN") == "1" }

const fixtureDSNVar = "ORM_FIXTURE_DSN"

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "fixtures"))
	if err != nil {
		t.Fatalf("resolving the fixture root: %v", err)
	}
	return root
}

func fixtureNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixtureRoot(t))
	if err != nil {
		t.Fatalf("reading the fixture root: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names
}

// runFixture applies the fixture's schema to a throwaway database, loads its
// configuration against it and reconciles.
func runFixture(t *testing.T, name string) *gen.Result {
	t.Helper()
	dir := filepath.Join(fixtureRoot(t), name)

	ddl, err := os.ReadFile(filepath.Join(dir, "schema.sql"))
	if err != nil {
		t.Fatalf("reading the fixture schema: %v", err)
	}
	dsn := testdb.Create(t, string(ddl))
	t.Setenv(fixtureDSNVar, dsn)

	cfg, err := config.Load(filepath.Join(dir, "orm.yaml"))
	if err != nil {
		t.Fatalf("loading the fixture configuration: %v", err)
	}
	result, err := gen.Check(t.Context(), cfg)
	if err != nil {
		t.Fatalf("gen.Check: %v", err)
	}
	return result
}

func render(t *testing.T, r *diag.Report) map[string]string {
	t.Helper()
	out := make(map[string]string, 3)
	for name, fn := range map[string]func(*bytes.Buffer, *diag.Report) error{
		"want.txt":    func(b *bytes.Buffer, r *diag.Report) error { return diag.RenderText(b, r) },
		"want.json":   func(b *bytes.Buffer, r *diag.Report) error { return diag.RenderJSON(b, r) },
		"want.github": func(b *bytes.Buffer, r *diag.Report) error { return diag.RenderGitHub(b, r) },
	} {
		var b bytes.Buffer
		if err := fn(&b, r); err != nil {
			t.Fatalf("rendering %s: %v", name, err)
		}
		out[name] = b.String()
	}
	return out
}

func TestFixtures(t *testing.T) {
	testdb.AdminDSN(t)
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			result := runFixture(t, name)
			got := render(t, result.Report)
			dir := filepath.Join(fixtureRoot(t), name)

			for _, golden := range []string{"want.txt", "want.json", "want.github"} {
				path := filepath.Join(dir, golden)
				if update {
					if err := os.WriteFile(path, []byte(got[golden]), 0o644); err != nil {
						t.Fatalf("writing %s: %v", golden, err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading %s (run with ORM_UPDATE_GOLDEN=1 to create it): %v", golden, err)
				}
				if string(want) != got[golden] {
					t.Errorf("%s does not match the golden file.\n--- got ---\n%s\n--- want ---\n%s", golden, got[golden], want)
				}
			}
		})
	}
}

func TestFixtures_deterministic(t *testing.T) {
	testdb.AdminDSN(t)
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			var first map[string]string
			for run := range 3 {
				got := render(t, runFixture(t, name).Report)
				if run == 0 {
					first = got
					continue
				}
				for _, format := range []string{"want.txt", "want.json", "want.github"} {
					if got[format] != first[format] {
						t.Fatalf("run %d produced different %s bytes than run 1", run+1, format)
					}
				}
			}
		})
	}
}

// codes returns the finding codes a fixture produced, in report order.
func codes(r *diag.Report) []string {
	var out []string
	for _, f := range r.Findings() {
		out = append(out, string(f.Code))
	}
	return out
}

func has(r *diag.Report, code diag.Code) bool {
	return slices.Contains(codes(r), string(code))
}

// findingsFor returns the findings of one code, in report order.
func findingsFor(r *diag.Report, code diag.Code) []diag.Finding {
	var out []diag.Finding
	for _, f := range r.Findings() {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

func TestClean_hasNoFindings(t *testing.T) {
	testdb.AdminDSN(t)
	r := runFixture(t, "01_clean").Report
	if r.Len() != 0 {
		t.Errorf("the clean fixture produced findings: %v", codes(r))
	}
}

func TestBelongsTo_doesNotRequireLocalUniqueness(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "04_belongs_to")

	if has(result.Report, diag.E010) {
		t.Errorf("a belongs-to relation over a non-unique foreign key produced E010: %v", findingsFor(result.Report, diag.E010))
	}
	rel := relation(t, result.Mapping, "Post", "Author")
	if rel.FKSide != model.FKLocal {
		t.Errorf("Post.Author resolved to the %s side, want local", rel.FKSide)
	}
	if rel.Cardinality != model.CardOne {
		t.Errorf("Post.Author cardinality = %v, want one", rel.Cardinality)
	}
}

func TestHasOne_requiresRemoteUniqueness(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "05_has_one")

	// User.Profile is backed by a total unique constraint and must resolve.
	rel := relation(t, result.Mapping, "User", "Profile")
	if rel.FKSide != model.FKRemote {
		t.Errorf("User.Profile resolved to the %s side, want remote", rel.FKSide)
	}

	// User.Session has no unique constraint at all.
	// User.Badge has only a partial unique index, which proves nothing.
	got := make(map[string]string)
	for _, f := range findingsFor(result.Report, diag.E010) {
		got[f.Field] = f.Reason
	}
	for _, field := range []string{"Session", "Badge"} {
		if _, ok := got[field]; !ok {
			t.Errorf("User.%s produced no E010; the relation claims at most one row and nothing guarantees it", field)
		}
	}
	if _, ok := got["Profile"]; ok {
		t.Error("User.Profile produced E010 despite a total unique constraint")
	}
	if reason := got["Badge"]; !strings.Contains(reason, "partial") {
		t.Errorf("the E010 on User.Badge reads %q, want it to name the partial index as the reason", reason)
	}
}

func TestHasMany_rejectsALocalForeignKey(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "06_has_many")

	rel := relation(t, result.Mapping, "User", "Posts")
	if rel.FKSide != model.FKRemote || rel.Cardinality != model.CardMany {
		t.Errorf("User.Posts = (%v, %v), want (many, remote)", rel.Cardinality, rel.FKSide)
	}

	var found bool
	for _, f := range findingsFor(result.Report, diag.E019) {
		if f.Field == "Orgs" {
			found = true
		}
	}
	if !found {
		t.Errorf("an orm.Many over a local foreign key produced no E019; got %v", codes(result.Report))
	}
}

func TestSideTag_pointingAtTheEmptySideBlamesTheTag(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "07_ambiguous")

	// Post.Backwards asks for side:remote, but every foreign key between posts
	// and users is on posts. The schema is fine and the tag is wrong, so the
	// finding must not claim there is no foreign key.
	var found bool
	for _, f := range findingsFor(result.Report, diag.E008) {
		if f.Field != "Backwards" {
			continue
		}
		found = true
		if !strings.Contains(f.Reason, "posts_author_fkey") {
			t.Errorf("reason = %q, want it to name the foreign key the tag excluded", f.Reason)
		}
		if !strings.Contains(f.Reason, "side:remote excludes") {
			t.Errorf("reason = %q, want it to blame the tag", f.Reason)
		}
		if !strings.Contains(f.Fix, "side:local") {
			t.Errorf("fix = %q, want it to suggest the other side", f.Fix)
		}
	}
	if !found {
		t.Errorf("a side: tag excluding every candidate produced no E008; got %v", codes(result.Report))
	}

	// The genuinely keyless case still reads as such.
	for _, f := range findingsFor(result.Report, diag.E008) {
		if f.Field == "Tag" && !strings.Contains(f.Reason, "without a foreign key") {
			t.Errorf("the keyless relation reads %q, want the schema-level reason", f.Reason)
		}
	}
}

func TestSelfReference_cardinalityDecidesDirection(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "08_self_ref")

	// One self-referencing foreign key is visible from both sides at once, so
	// counting candidates cannot decide anything. Cardinality does.
	manager := relation(t, result.Mapping, "Employee", "Manager")
	if manager.FKSide != model.FKLocal {
		t.Errorf("Employee.Manager resolved to the %s side, want local", manager.FKSide)
	}
	reports := relation(t, result.Mapping, "Employee", "Reports")
	if reports.FKSide != model.FKRemote {
		t.Errorf("Employee.Reports resolved to the %s side, want remote", reports.FKSide)
	}
	if manager.FK != reports.FK {
		t.Error("the two relations resolved to different constraints; there is only one")
	}

	// KeyCols and TargetCols swap with the side, so a consumer never branches
	// on FKSide.
	if got := keyNames(manager.KeyCols); got != "manager_id" {
		t.Errorf("Manager.KeyCols = %q, want manager_id", got)
	}
	if got := keyNames(manager.TargetCols); got != "id" {
		t.Errorf("Manager.TargetCols = %q, want id", got)
	}
	if got := keyNames(reports.KeyCols); got != "id" {
		t.Errorf("Reports.KeyCols = %q, want id", got)
	}
	if got := keyNames(reports.TargetCols); got != "manager_id" {
		t.Errorf("Reports.TargetCols = %q, want manager_id", got)
	}
}

func TestSelfReference_sideTagIsHonoured(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "08_self_ref")

	// Employee.Deputy is orm.One with side:remote over the one self-reference.
	// Both candidate lists hold that same constraint, so a side filter that
	// emptied one of them would report E008 instead of resolving.
	for _, f := range findingsFor(result.Report, diag.E008) {
		if f.Field == "Deputy" {
			t.Fatalf("side:remote on a self-reference produced E008: %s", f.Reason)
		}
	}
	// Having resolved to the remote side it is a has-one, and
	// employees.manager_id is not unique, so the uniqueness rule must fire.
	var found bool
	for _, f := range findingsFor(result.Report, diag.E010) {
		if f.Field == "Deputy" {
			found = true
		}
	}
	if !found {
		t.Errorf("side:remote on a self-reference did not apply the has-one uniqueness rule; got %v", codes(result.Report))
	}
}

func TestSelfReference_ambiguityNeedsAConstraintName(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "08_self_ref")

	var found bool
	for _, f := range findingsFor(result.Report, diag.E009) {
		if f.Field == "Parent" {
			found = true
			if !strings.Contains(f.Fix, "side:local or side:remote") {
				t.Errorf("the E009 fix reads %q, want it to mention the side tag too", f.Fix)
			}
		}
	}
	if !found {
		t.Errorf("two self-referencing foreign keys produced no E009; got %v", codes(result.Report))
	}
	// The pinned ones resolve.
	if got := relation(t, result.Mapping, "Node", "Origin").FK.Name; got != "nodes_origin_fkey" {
		t.Errorf("Node.Origin resolved to %s", got)
	}
	if got := relation(t, result.Mapping, "Node", "Children").FK.Name; got != "nodes_parent_fkey" {
		t.Errorf("Node.Children resolved to %s", got)
	}
}

func TestCompositeForeignKey_preservesElementOrder(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "09_composite")

	rel := relation(t, result.Mapping, "Post", "Author")
	// posts(tenant_id, author_id) references users(tenant_id, id). The two
	// sides are in different relative column order on their tables, so a set
	// comparison would pair author_id with tenant_id and look correct.
	if got := keyNames(rel.KeyCols); got != "tenant_id,author_id" {
		t.Errorf("KeyCols = %q, want tenant_id,author_id in constraint order", got)
	}
	if got := keyNames(rel.TargetCols); got != "tenant_id,id" {
		t.Errorf("TargetCols = %q, want tenant_id,id in constraint order", got)
	}
	if len(rel.KeyCols) != len(rel.TargetCols) {
		t.Fatalf("%d key columns against %d target columns", len(rel.KeyCols), len(rel.TargetCols))
	}
}

func TestUnmappedNullableForeignKey_isAllowed(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "17_unmapped_keys")

	rel := relation(t, result.Mapping, "Post", "Author")
	if len(rel.KeyCols) != 1 {
		t.Fatalf("Post.Author has %d key columns, want 1", len(rel.KeyCols))
	}
	if rel.KeyCols[0].FieldIdx != -1 {
		t.Errorf("FieldIdx = %d, want -1: posts.author_id has no mapped Go field", rel.KeyCols[0].FieldIdx)
	}
	if rel.KeyCols[0].Column == nil || rel.KeyCols[0].Column.Name != "author_id" {
		t.Errorf("the key column is %v, want posts.author_id; the column is the source of truth", rel.KeyCols[0].Column)
	}

	// The only finding about that column is W003, never a new error invented
	// for the unmapped key.
	for _, f := range result.Report.Findings() {
		if f.Column == "author_id" && f.Code != diag.W003 {
			t.Errorf("posts.author_id produced %s; an intentionally unmapped nullable key is a warning at most", f.Code)
		}
	}
}

func TestUnmappedForeignKey_warningNamesTheRelation(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "17_unmapped_keys")
	for _, f := range findingsFor(result.Report, diag.W003) {
		if f.Column != "author_id" {
			continue
		}
		if !strings.Contains(f.Reason, "carries the foreign key for") {
			t.Errorf("W003 on author_id reads %q, want it to name the relation it feeds", f.Reason)
		}
		if !strings.Contains(f.Reason, "read-only") {
			t.Errorf("W003 on author_id reads %q, want it to state the consequence", f.Reason)
		}
		return
	}
	t.Error("no W003 for the unmapped relation key column")
}

func TestUnmappedRequiredForeignKey_isAnError(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "17_unmapped_keys")
	for _, f := range findingsFor(result.Report, diag.E002) {
		if f.Column == "owner_id" {
			return
		}
	}
	t.Errorf("a NOT NULL foreign key with no default and no mapped field produced no E002; got %v", codes(result.Report))
}

func TestUnmappedPrimaryKey_isAnError(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "17_unmapped_keys")
	found := findingsFor(result.Report, diag.E023)
	if len(found) == 0 {
		t.Fatalf("an entity with no field for its primary key produced no E023; got %v", codes(result.Report))
	}
	if found[0].Entity != "domain.Note" {
		t.Errorf("E023 reported on %s, want domain.Note", found[0].Entity)
	}
}

func TestCompositeUnmappedKeys_mixedFieldIndexInOrder(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "17_unmapped_keys")

	rel := relation(t, result.Mapping, "Comment", "Author")
	if len(rel.KeyCols) != 2 {
		t.Fatalf("Comment.Author has %d key columns, want 2", len(rel.KeyCols))
	}
	if got := keyNames(rel.KeyCols); got != "tenant_id,author_id" {
		t.Errorf("KeyCols = %q, want tenant_id,author_id", got)
	}
	if !rel.KeyCols[0].Mapped() {
		t.Error("comments.tenant_id is mapped but reported FieldIdx -1")
	}
	if rel.KeyCols[1].Mapped() {
		t.Error("comments.author_id is not mapped but reported a field index")
	}

	em := entityMapping(t, result.Mapping, "Comment")
	if got := em.Cols[rel.KeyCols[0].FieldIdx].Column.Name; got != "tenant_id" {
		t.Errorf("FieldIdx %d points at %s, want tenant_id", rel.KeyCols[0].FieldIdx, got)
	}
}

func TestDuplicateMappings(t *testing.T) {
	testdb.AdminDSN(t)
	r := runFixture(t, "13_duplicates").Report
	if !has(r, diag.E017) {
		t.Errorf("two entities over one table produced no E017; got %v", codes(r))
	}
	if !has(r, diag.E018) {
		t.Errorf("two fields over one column produced no E018; got %v", codes(r))
	}
}

// Views reconcile. This fixture asserted E022 — "view entity unsupported" —
// which was the reserved-directive placeholder M16.5 exists to replace.
//
// What it asserts now is that a view declared over a real view reconciles
// without any of the findings that are true only of tables. A view has no
// primary key and PostgreSQL allows none, so E011 would be a finding whose
// suggested fix is impossible; and PostgreSQL records no NOT NULL on any view
// column, so E004 would fire on every field of every view while proving
// nothing — the catalog's answer there is "unknown", not "nullable".
func TestViews(t *testing.T) {
	testdb.AdminDSN(t)
	r := runFixture(t, "14_views").Report
	if has(r, diag.E022) {
		t.Errorf("a view entity is still reported as unsupported: %v", codes(r))
	}
	for _, tableOnly := range []diag.Code{diag.E011, diag.E004, diag.E023} {
		if has(r, tableOnly) {
			t.Errorf("%s was reported against a view; it is true only of tables: %v",
				tableOnly, codes(r))
		}
	}
}

func TestInvalidTags(t *testing.T) {
	testdb.AdminDSN(t)
	r := runFixture(t, "15_tags").Report
	found := findingsFor(r, diag.E021)
	if len(found) == 0 {
		t.Fatalf("no E021 for the invalid tags; got %v", codes(r))
	}
	fields := make(map[string]bool, len(found))
	for _, f := range found {
		fields[f.Field] = true
	}
	for _, want := range []string{"Unknown", "FKOnScalar", "BadSide"} {
		if !fields[want] {
			t.Errorf("field %s produced no E021", want)
		}
	}
}

func TestRelationTargetMustBeAnEntity(t *testing.T) {
	testdb.AdminDSN(t)
	r := runFixture(t, "18_relation_targets").Report
	if !has(r, diag.E020) {
		t.Errorf("a relation to an unmarked struct produced no E020; got %v", codes(r))
	}
}

func TestGeneratedIdentifierCollision(t *testing.T) {
	testdb.AdminDSN(t)
	r := runFixture(t, "19_identifiers").Report
	if !has(r, diag.E014) {
		t.Errorf("two same-named entities generating into one directory produced no E014; got %v", codes(r))
	}
}

func TestNumericAndUUIDNeedConfiguration(t *testing.T) {
	testdb.AdminDSN(t)
	r := runFixture(t, "10_types").Report
	var cols []string
	for _, f := range findingsFor(r, diag.E013) {
		cols = append(cols, f.Column)
	}
	slices.Sort(cols)
	if got := strings.Join(cols, ","); got != "amount,ref" {
		t.Errorf("E013 columns = %q, want amount,ref: numeric and uuid never map by default", got)
	}
}

func TestConfiguredTypesReconcile(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "16_overrides")

	for _, f := range result.Report.Findings() {
		if f.Code == diag.E013 {
			t.Errorf("a configured type still reported E013: %+v", f)
		}
	}

	// The column: tag reroutes a field to a differently named column.
	em := entityMapping(t, result.Mapping, "Payment")
	var name *model.ColMapping
	for i := range em.Cols {
		if em.Cols[i].Field.Name == "Name" {
			name = &em.Cols[i]
		}
	}
	if name == nil {
		t.Fatal("Payment.Name was not mapped")
	}
	if name.Column.Name != "legacy_name" {
		t.Errorf("Payment.Name mapped to %s, want legacy_name", name.Column.Name)
	}
}

func TestTypeTag_selectsAConfiguredMapping(t *testing.T) {
	testdb.AdminDSN(t)
	result := runFixture(t, "16_overrides")

	// Payment.Fee and Payment.FeeUntagged are the same Go struct against the
	// same column shape. Only the tagged one maps, which is what makes the tag
	// load-bearing rather than decorative.
	byField := make(map[string]diag.Code)
	for _, f := range findingsFor(result.Report, diag.E006) {
		byField[f.Field] = f.Code
	}
	if _, ok := byField["Fee"]; ok {
		t.Error("the type: tag did not select the configured mapping for Payment.Fee")
	}
	if _, ok := byField["FeeUntagged"]; !ok {
		t.Errorf("Payment.FeeUntagged has no configured mapping and should not reconcile against text; got %v", codes(result.Report))
	}
}

func TestStrict_configuresTheSeverityOfSoftFindings(t *testing.T) {
	testdb.AdminDSN(t)

	// 20_warnings_only and 21_strict hold the same entities over the same
	// schema. Only the strict: policy differs.
	relaxed := runFixture(t, "20_warnings_only").Report
	strict := runFixture(t, "21_strict").Report

	if got := codes(relaxed); len(got) != 2 {
		t.Fatalf("the default policy produced %v, want W003 and W015", got)
	}
	if relaxed.Count(diag.SeverityError) != 0 {
		t.Error("the default policy produced an error; both findings are warnings by default")
	}

	// unmapped_columns: error raises W003 without changing its code, because
	// codes are identity and severity is policy.
	found := findingsFor(strict, diag.W003)
	if len(found) != 1 {
		t.Fatalf("strict produced %d W003 findings, want 1", len(found))
	}
	if found[0].Severity != diag.SeverityError {
		t.Errorf("W003 severity under unmapped_columns: error = %v, want error", found[0].Severity)
	}

	// timestamp_without_tz: off suppresses W015 entirely.
	if has(strict, diag.W015) {
		t.Error("timestamp_without_tz: off did not suppress W015")
	}
}

// relation looks a relation mapping up by entity and field name.
func relation(t *testing.T, m *model.Mapping, entity, field string) model.RelMapping {
	t.Helper()
	em := entityMapping(t, m, entity)
	for _, rel := range em.Rels {
		if rel.Field.Name == field {
			return rel
		}
	}
	t.Fatalf("%s has no resolved relation %s", entity, field)
	return model.RelMapping{}
}

func entityMapping(t *testing.T, m *model.Mapping, entity string) *model.EntityMapping {
	t.Helper()
	for _, em := range m.Entities {
		if em.Entity.Name == entity {
			return em
		}
	}
	t.Fatalf("%s was not mapped", entity)
	return nil
}

func keyNames(keys []model.RelKeyCol) string {
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k.Column.Name)
	}
	return strings.Join(names, ",")
}

// A relation whose target is in another package is E024.
//
// Two things are being pinned here. The first is that it is reported at all:
// generation refuses such a relation because the target's descriptors are
// unexported in the target's package, and before E024 existed that refusal came
// only from the emitter — after `orm makemigrations` and `orm migrate` had
// already run, since neither of those reconciles. The developer found out last.
//
// The second is that everything else about the fixture is clean. The tables
// exist, the columns match and the foreign key is real: the relation is the
// only finding, which is what makes the message worth reading.
func TestCrossPackageRelation(t *testing.T) {
	testdb.AdminDSN(t)
	r := runFixture(t, "22_cross_package").Report

	if !has(r, diag.E024) {
		t.Fatalf("a relation across packages produced no E024; got %v", codes(r))
	}
	for _, f := range r.Findings() {
		if f.Code != diag.E024 {
			t.Errorf("unexpected %s: %s", f.Code, f.Message)
		}
	}

	var found diag.Finding
	for _, f := range r.Findings() {
		if f.Code == diag.E024 {
			found = f
			break
		}
	}
	// The message has to name both packages. "unsupported relation" would send
	// the reader looking for which one.
	for _, want := range []string{"sales.Order", "catalog.Product"} {
		if !strings.Contains(found.Message, want) {
			t.Errorf("the message does not name %s: %s", want, found.Message)
		}
	}
	if found.Fix == "" {
		t.Error("the finding offers no fix")
	}
	if found.Code.Severity() != diag.SeverityError {
		t.Error("E024 is not an error, but generation cannot proceed past it")
	}
}
