package goscan_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/goscan"
	"github.com/AlexAli29/orm/internal/gen/model"
)

func scan(t *testing.T, dirs ...string) *goscan.Result {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving the test root: %v", err)
	}
	targets := make([]goscan.Target, 0, len(dirs))
	for _, d := range dirs {
		abs := filepath.Join(root, "testdata", d)
		targets = append(targets, goscan.Target{Dir: abs, OutputDir: abs})
	}
	res, err := goscan.Scan(t.Context(), root, targets)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return res
}

func entity(t *testing.T, res *goscan.Result, name string) *model.GoEntity {
	t.Helper()
	for _, e := range res.Entities {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("entity %s not discovered", name)
	return nil
}

func field(t *testing.T, e *model.GoEntity, name string) *model.GoField {
	t.Helper()
	for i := range e.Fields {
		if e.Fields[i].Name == name {
			return &e.Fields[i]
		}
	}
	t.Fatalf("%s has no field %s", e.Name, name)
	return nil
}

func TestScan_discoversMarkedStructsOnly(t *testing.T) {
	res := scan(t, "basic")
	var names []string
	for _, e := range res.Entities {
		names = append(names, e.Name)
	}
	const want = "ActiveUser,Event,Post,Profile,User"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("entities = %q, want %q (Unmarked carries no directive)", got, want)
	}
}

func TestScan_bothDocCommentPlacements(t *testing.T) {
	res := scan(t, "basic")
	// User is an ungrouped declaration, whose doc attaches to the GenDecl.
	if got := entity(t, res, "User").Table.String(); got != "users" {
		t.Errorf("User table = %q, want users", got)
	}
	// Post sits inside a type ( ... ) group, whose doc attaches to the TypeSpec.
	if got := entity(t, res, "Post").Table.String(); got != "posts" {
		t.Errorf("Post table = %q, want posts (grouped declaration)", got)
	}
}

func TestScan_qualifiedTableName(t *testing.T) {
	e := entity(t, scan(t, "basic"), "Event")
	if e.Table.Schema != "analytics" || e.Table.Name != "events" {
		t.Errorf("Event table = %+v, want analytics.events", e.Table)
	}
}

func TestScan_viewIsDiscoveredAndFlagged(t *testing.T) {
	e := entity(t, scan(t, "basic"), "ActiveUser")
	if !(e.Kind == model.RelView) {
		t.Error("an //orm:view entity was not flagged as a view")
	}
	if e.Table.Name != "active_users" {
		t.Errorf("view table = %q", e.Table.Name)
	}
}

func TestScan_positionsAreRelative(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")
	if e.Pos.File != "testdata/basic/entities.go" {
		t.Errorf("position file = %q, want a path relative to the root", e.Pos.File)
	}
	if e.Pos.Line == 0 {
		t.Error("position has no line")
	}
	if e.Marker.Line >= e.Pos.Line {
		t.Errorf("marker at line %d, type at line %d; the directive must precede the type", e.Marker.Line, e.Pos.Line)
	}
}

func TestScan_unexportedFieldsAreNotMapped(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")
	for _, f := range e.Fields {
		if f.Name == "hidden" {
			t.Error("an unexported field was included in the mapped surface")
		}
	}
}

func TestScan_scalarTypes(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")
	tests := []struct {
		field    string
		kind     model.GoKind
		ptr      bool
		sqlNull  bool
		nullable bool
		named    string
		src      string
	}{
		{field: "ID", kind: model.KindInt64, src: "int64"},
		{field: "Email", kind: model.KindString, src: "string"},
		{field: "Nickname", kind: model.KindString, ptr: true, nullable: true, src: "*string"},
		{field: "State", kind: model.KindString, named: "github.com/AlexAli29/orm/internal/gen/goscan/testdata/basic.Status", src: "Status"},
		{field: "Rank", kind: model.KindInt, named: "github.com/AlexAli29/orm/internal/gen/goscan/testdata/basic.Level", src: "Level"},
		{field: "Bio", kind: model.KindString, sqlNull: true, nullable: true, src: "sql.Null[string]"},
		{field: "Legacy", kind: model.KindString, sqlNull: true, nullable: true, src: "sql.NullString"},
		{field: "Tags", kind: model.KindSlice, src: "[]string"},
		{field: "Blob", kind: model.KindBytes, src: "[]byte"},
		{field: "Meta", kind: model.KindMap, nullable: true, src: "map[string]any"},
		{field: "CreatedAt", kind: model.KindTime, named: "time.Time", src: "time.Time"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			f := field(t, e, tt.field)
			if f.Type.Kind != tt.kind {
				t.Errorf("kind = %v, want %v", f.Type.Kind, tt.kind)
			}
			if f.Type.Ptr != tt.ptr {
				t.Errorf("Ptr = %v, want %v", f.Type.Ptr, tt.ptr)
			}
			if f.Type.SQLNull != tt.sqlNull {
				t.Errorf("SQLNull = %v, want %v", f.Type.SQLNull, tt.sqlNull)
			}
			if f.Type.Nullable() != tt.nullable {
				t.Errorf("Nullable = %v, want %v", f.Type.Nullable(), tt.nullable)
			}
			if f.Type.Named != tt.named {
				t.Errorf("Named = %q, want %q", f.Type.Named, tt.named)
			}
			if f.Type.Src != tt.src {
				t.Errorf("Src = %q, want %q", f.Type.Src, tt.src)
			}
		})
	}
}

func TestScan_plainSliceIsNotNullCapable(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")
	if field(t, e, "Tags").Type.Nullable() {
		t.Error("[]string reported null-capable; a nil slice and an empty array are the same value")
	}
	if field(t, e, "Blob").Type.Nullable() {
		t.Error("[]byte reported null-capable")
	}
}

func TestScan_sliceElement(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")
	tags := field(t, e, "Tags").Type
	if tags.Elem == nil {
		t.Fatal("[]string has no element type")
	}
	if tags.Elem.Kind != model.KindString {
		t.Errorf("element kind = %v, want string", tags.Elem.Kind)
	}
}

func TestScan_enumConstants(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")

	state := field(t, e, "State").Type
	if !state.IsEnum() {
		t.Fatal("Status has constants but was not treated as an enum")
	}
	var labels []string
	for _, c := range state.Enum {
		labels = append(labels, c.Value())
	}
	if got := strings.Join(labels, ","); got != "pending,active" {
		t.Errorf("Status labels = %q, want pending,active in declaration order", got)
	}

	rank := field(t, e, "Rank").Type
	if len(rank.Enum) != 2 {
		t.Fatalf("Level has %d constants, want 2", len(rank.Enum))
	}
	if rank.Enum[0].IsString || rank.Enum[0].Int != 1 {
		t.Errorf("Level constant = %+v, want the integer 1", rank.Enum[0])
	}

	if field(t, e, "Untyped").Type.IsEnum() {
		t.Error("a named string type with no constants was treated as an enum")
	}
}

func TestScan_relationDetection(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")

	posts := field(t, e, "Posts")
	if posts.Rel == nil {
		t.Fatal("orm.Many field was not detected as a relation")
	}
	if posts.Rel.Cardinality != model.CardMany {
		t.Errorf("Posts cardinality = %v, want many", posts.Rel.Cardinality)
	}
	const wantTarget = "github.com/AlexAli29/orm/internal/gen/goscan/testdata/basic.Post"
	if posts.Rel.Target != wantTarget {
		t.Errorf("Posts target = %q, want %q", posts.Rel.Target, wantTarget)
	}

	profile := field(t, e, "Profile")
	if profile.Rel == nil || profile.Rel.Cardinality != model.CardOne {
		t.Fatalf("Profile relation = %+v, want cardinality one", profile.Rel)
	}

	if field(t, e, "Email").Rel != nil {
		t.Error("a scalar field was detected as a relation")
	}
}

func TestScan_relationCarriesNoDirection(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")
	// GoRel has exactly two fields; direction is established during
	// reconciliation, from the catalog. The side: tag is a hint stored on the
	// tags, not on the relation.
	posts := field(t, e, "Posts")
	if posts.Tags.HasSide {
		t.Error("Posts carries a side: tag it was not given")
	}
	profile := field(t, e, "Profile")
	if !profile.Tags.HasSide || profile.Tags.Side != model.FKRemote {
		t.Errorf("Profile side tag = (%v, %v), want remote", profile.Tags.HasSide, profile.Tags.Side)
	}
}

func TestScan_tagDirectives(t *testing.T) {
	e := entity(t, scan(t, "basic"), "User")

	if got := field(t, e, "Email").Tags.Column; got != "email_address" {
		t.Errorf("column tag = %q, want email_address", got)
	}
	if got := field(t, e, "Posts").Tags.FK; got != "posts_author_fkey" {
		t.Errorf("fk tag = %q", got)
	}
	manager := field(t, e, "Manager")
	if manager.Tags.FK != "users_manager_fkey" || !manager.Tags.HasSide || manager.Tags.Side != model.FKLocal {
		t.Errorf("Manager tags = %+v, want fk and side:local", manager.Tags)
	}
	if !field(t, e, "Ignored").Tags.Ignore {
		t.Error(`orm:"-" did not mark the field ignored`)
	}
}

func TestScan_aliasedImport(t *testing.T) {
	res := scan(t, "aliased")
	user := entity(t, res, "User")

	posts := field(t, user, "Posts")
	if posts.Rel == nil || posts.Rel.Cardinality != model.CardMany {
		t.Error("a relation imported under an alias was not detected")
	}
	if field(t, user, "Decoy").Rel != nil {
		t.Error("a local type named One was mistaken for the relation type")
	}
	post := entity(t, res, "Post")
	if field(t, post, "Author").Rel == nil {
		t.Error("aliased orm.One was not detected")
	}
	if field(t, post, "Decoys").Rel != nil {
		t.Error("a local type named Many was mistaken for the relation type")
	}
}

func TestScan_dotImport(t *testing.T) {
	res := scan(t, "dotimport")
	if field(t, entity(t, res, "User"), "Posts").Rel == nil {
		t.Error("a dot-imported Many was not detected as a relation")
	}
	if field(t, entity(t, res, "Post"), "Author").Rel == nil {
		t.Error("a dot-imported One was not detected as a relation")
	}
}

func TestScan_tagErrors(t *testing.T) {
	res := scan(t, "badtags")
	got := make(map[string]string, len(res.TagErrors))
	for _, e := range res.TagErrors {
		got[e.Field] = e.Err.Error()
	}

	want := map[string]string{
		"Unknown":   "unknown directive",
		"NoValue":   "has no value",
		"EmptyVal":  "needs a value",
		"FKOnField": "only valid on an orm.One or orm.Many field",
		"SideOnCol": "only valid on an orm.One or orm.Many field",
		"Duplicate": "duplicate column directive",
		"Stray":     "empty directive",
		"BadSide":   `invalid side "sideways"`,
		"ColOnRel":  "not valid on a relation field",
		"Empty":     "empty tag",
	}
	for fieldName, fragment := range want {
		msg, ok := got[fieldName]
		if !ok {
			t.Errorf("%s produced no tag error, want one containing %q", fieldName, fragment)
			continue
		}
		if !strings.Contains(msg, fragment) {
			t.Errorf("%s tag error = %q, want it to contain %q", fieldName, msg, fragment)
		}
	}
	for fieldName := range got {
		if _, ok := want[fieldName]; !ok {
			t.Errorf("unexpected tag error on %s: %s", fieldName, got[fieldName])
		}
	}
}

func TestScan_tagErrorsCarryPositions(t *testing.T) {
	res := scan(t, "badtags")
	if len(res.TagErrors) == 0 {
		t.Fatal("no tag errors")
	}
	for _, e := range res.TagErrors {
		if e.Pos.File != "testdata/badtags/entities.go" || e.Pos.Line == 0 {
			t.Errorf("%s has position %q, want a located, relative path", e.Field, e.Pos)
		}
		if e.Entity != "badtags.User" {
			t.Errorf("%s reports entity %q", e.Field, e.Entity)
		}
	}
}

func TestScan_deterministicOrder(t *testing.T) {
	render := func() string {
		res := scan(t, "basic", "aliased", "dotimport")
		var b strings.Builder
		for _, e := range res.Entities {
			b.WriteString(e.Qualified())
			b.WriteByte('\n')
			for _, f := range e.Fields {
				b.WriteString("  ")
				b.WriteString(f.Name)
				b.WriteByte(' ')
				b.WriteString(f.Type.Src)
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	first := render()
	for i := range 2 {
		if got := render(); got != first {
			t.Fatalf("scan run %d differed from the first", i+2)
		}
	}
}

func TestScan_outputDirIsRecorded(t *testing.T) {
	res := scan(t, "basic")
	e := entity(t, res, "User")
	if !strings.HasSuffix(filepath.ToSlash(e.OutputDir), "testdata/basic") {
		t.Errorf("OutputDir = %q", e.OutputDir)
	}
}

func TestScan_noTargets(t *testing.T) {
	if _, err := goscan.Scan(t.Context(), ".", nil); err == nil {
		t.Error("Scan with no targets succeeded, want an error")
	}
}

// scanErr runs Scan expecting it to fail, and returns the error.
func scanErr(t *testing.T, dir string) error {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving the test root: %v", err)
	}
	abs := filepath.Join(root, "testdata", dir)
	_, err = goscan.Scan(t.Context(), root, []goscan.Target{{Dir: abs, OutputDir: abs}})
	if err == nil {
		t.Fatalf("Scan(%s) succeeded, want an error", dir)
	}
	return err
}

func TestScan_packageThatDoesNotTypeCheck(t *testing.T) {
	// A findings report derived from unresolved types would be worse than no
	// report: every field of an unknown type would produce a mismatch the
	// author cannot act on.
	err := scanErr(t, "broken")
	if !strings.Contains(err.Error(), "do not type-check") {
		t.Errorf("error = %v, want it to say the packages do not type-check", err)
	}
	if !strings.Contains(err.Error(), "NoSuchType") {
		t.Errorf("error = %v, want it to carry the compiler's own message", err)
	}
}

func TestScan_directiveOnANonStruct(t *testing.T) {
	err := scanErr(t, "nonstruct")
	if !strings.Contains(err.Error(), "is not a struct") {
		t.Errorf("error = %v, want it to say the entity is not a struct", err)
	}
	if !strings.Contains(err.Error(), "testdata/nonstruct/entities.go") {
		t.Errorf("error = %v, want it to carry a relative source position", err)
	}
}

func TestScan_directoryThatDoesNotExist(t *testing.T) {
	// A package that does not exist and a package that does not compile send
	// the reader to different places, so they are worded apart.
	err := scanErr(t, "no_such_package")
	if !strings.Contains(err.Error(), "could not be loaded") {
		t.Errorf("error = %v, want the load wording rather than the type-check one", err)
	}
	if strings.Contains(err.Error(), "type-check") {
		t.Errorf("error = %v, blames type checking for a directory that is not there", err)
	}
}

func TestParseTags(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		want    model.FieldTags
		wantErr string
	}{
		{name: "empty struct tag", tag: ``},
		{name: "other keys only", tag: `json:"a"`},
		{name: "column", tag: `orm:"column:email_address"`, want: model.FieldTags{Column: "email_address", Raw: "column:email_address"}},
		{name: "ignore", tag: `orm:"-"`, want: model.FieldTags{Ignore: true, Raw: "-"}},
		{name: "fk and side", tag: `orm:"fk:c,side:remote"`, want: model.FieldTags{FK: "c", Side: model.FKRemote, HasSide: true, Raw: "fk:c,side:remote"}},
		{name: "type", tag: `orm:"type:money"`, want: model.FieldTags{Type: "money", Raw: "type:money"}},
		{name: "unknown", tag: `orm:"nope:1"`, wantErr: "unknown directive"},
		{name: "bad side", tag: `orm:"side:up"`, wantErr: "invalid side"},
		{name: "no value", tag: `orm:"column"`, wantErr: "has no value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := goscan.ParseTags(tt.tag)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTags(%q): %v", tt.tag, err)
			}
			if got != tt.want {
				t.Errorf("ParseTags(%q) = %+v, want %+v", tt.tag, got, tt.want)
			}
		})
	}
}
