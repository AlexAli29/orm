package lock_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/lock"
	"github.com/AlexAli29/orm/internal/gen/model"
)

// What the fingerprint is for is telling a change that matters from one that
// does not, so the tests come in two halves: changes that must move it, and
// changes that must not.

// The types are built per mapping rather than shared, so that a test which
// changes one is changing its own copy. A shared pointer here would let the
// first subtest decide what the later ones fingerprint.
func int8Type() *model.PGType { return &model.PGType{OID: 20, Schema: "pg_catalog", Name: "int8"} }
func int4Type() *model.PGType { return &model.PGType{OID: 23, Schema: "pg_catalog", Name: "int4"} }
func textType() *model.PGType { return &model.PGType{OID: 25, Schema: "pg_catalog", Name: "text"} }
func enumType() *model.PGType {
	return &model.PGType{OID: 16384, Schema: "public", Name: "user_state", Kind: model.PGEnum,
		Labels: []string{"pending", "active"}}
}

// mapping builds a two-entity mapping with a relation between them, which is
// enough surface for every part of the fingerprint to be exercised.
func mapping() *model.Mapping {
	users := &model.PGTable{Schema: "public", Name: "users"}
	posts := &model.PGTable{Schema: "public", Name: "posts"}

	userID := &model.PGColumn{Table: users, Name: "id", AttNum: 1, Type: int8Type(), NotNull: true, Identity: 'd'}
	userEmail := &model.PGColumn{Table: users, Name: "email", AttNum: 2, Type: textType(), NotNull: true}
	userState := &model.PGColumn{Table: users, Name: "state", AttNum: 3, Type: enumType(), NotNull: true, HasDefault: true}
	users.Cols = []*model.PGColumn{userID, userEmail, userState}
	users.PK = []*model.PGColumn{userID}

	postID := &model.PGColumn{Table: posts, Name: "id", AttNum: 1, Type: int8Type(), NotNull: true, Identity: 'd'}
	postAuthor := &model.PGColumn{Table: posts, Name: "author_id", AttNum: 2, Type: int8Type()}
	posts.Cols = []*model.PGColumn{postID, postAuthor}
	posts.PK = []*model.PGColumn{postID}

	fk := &model.PGForeignKey{
		Name: "posts_author_id_fkey", Table: posts, RefTable: users,
		Cols: []*model.PGColumn{postAuthor}, RefCols: []*model.PGColumn{userID},
	}
	posts.FKs = []*model.PGForeignKey{fk}

	userEntity := &model.GoEntity{Name: "User", PkgPath: "example.com/app/domain", PkgName: "domain"}
	postEntity := &model.GoEntity{Name: "Post", PkgPath: "example.com/app/domain", PkgName: "domain"}
	userEntity.Fields = []model.GoField{
		{Name: "ID", Type: model.GoType{Src: "int64", Value: "int64"}},
		{Name: "Email", Type: model.GoType{Src: "string", Value: "string"}},
		{Name: "State", Type: model.GoType{Src: "UserState", Value: "UserState"}},
		{Name: "Posts", Type: model.GoType{Src: "orm.Many[Post]"}},
	}
	postEntity.Fields = []model.GoField{
		{Name: "ID", Type: model.GoType{Src: "int64", Value: "int64"}},
		{Name: "AuthorID", Type: model.GoType{Src: "*int64", Value: "int64", Ptr: true}},
	}

	um := &model.EntityMapping{Entity: userEntity, Table: users}
	pm := &model.EntityMapping{Entity: postEntity, Table: posts}
	um.Cols = []model.ColMapping{
		{Field: &userEntity.Fields[0], Idx: 0, Column: userID},
		{Field: &userEntity.Fields[1], Idx: 1, Column: userEmail},
		{Field: &userEntity.Fields[2], Idx: 2, Column: userState},
	}
	pm.Cols = []model.ColMapping{
		{Field: &postEntity.Fields[0], Idx: 0, Column: postID},
		{Field: &postEntity.Fields[1], Idx: 1, Column: postAuthor},
	}
	um.Rels = []model.RelMapping{{
		Field: &userEntity.Fields[3], Idx: 3, Cardinality: model.CardMany, FKSide: model.FKRemote, FK: fk,
		KeyCols:    []model.RelKeyCol{{Column: userID, FieldIdx: 0}},
		TargetCols: []model.RelKeyCol{{Column: postAuthor, FieldIdx: 1}},
		Target:     pm,
	}}
	return &model.Mapping{Entities: []*model.EntityMapping{um, pm}}
}

func TestFingerprint_isDeterministic(t *testing.T) {
	want := lock.Fingerprint(mapping())
	if len(want) != 64 {
		t.Fatalf("fingerprint = %q, want a sha256 digest", want)
	}
	// Rebuilt from scratch each time, so nothing about pointer identity or
	// allocation order can be leaking into it.
	for range 20 {
		if got := lock.Fingerprint(mapping()); got != want {
			t.Fatalf("fingerprint = %s, want %s", got, want)
		}
	}
}

// Everything in this list decides something about generated code, so a change
// to any of it makes committed code wrong.
func TestFingerprint_changesThatMatter(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*model.Mapping)
	}{
		{
			name:  "a mapped column's PostgreSQL type",
			apply: func(m *model.Mapping) { m.Entities[0].Cols[1].Column.Type = int4Type() },
		},
		{
			name:  "a mapped column's Go type",
			apply: func(m *model.Mapping) { m.Entities[0].Cols[1].Field.Type.Src = "*string" },
		},
		{
			// M12.2. The two instantiations share an origin, so a fingerprint
			// taken over the origin's name alone would call them one type and
			// an int8range column would silently keep an int32 mapping. The
			// fingerprint reads the type as written, which carries the argument.
			name: "a generic instantiation's type argument",
			apply: func(m *model.Mapping) {
				c := &m.Entities[0].Cols[1]
				c.Field.Type = model.GoType{
					Src: "orm.Range[int32]", Value: "orm.Range[int32]",
					Named:    "github.com/AlexAli29/orm.Range",
					Kind:     model.KindStruct,
					TypeArgs: []model.GoType{{Src: "int32", Value: "int32", Kind: model.KindInt32}},
				}
			},
		},
		{
			name:  "nullability",
			apply: func(m *model.Mapping) { m.Entities[0].Cols[1].Column.NotNull = false },
		},
		{
			// Whether an insert may leave a column out is generated metadata.
			name:  "a default",
			apply: func(m *model.Mapping) { m.Entities[0].Cols[1].Column.HasDefault = true },
		},
		{
			name:  "identity",
			apply: func(m *model.Mapping) { m.Entities[0].Cols[0].Column.Identity = 0 },
		},
		{
			name:  "a generated column",
			apply: func(m *model.Mapping) { m.Entities[0].Cols[1].Column.Generated = 's' },
		},
		{
			name:  "the primary key",
			apply: func(m *model.Mapping) { m.Entities[0].Table.PK = nil },
		},
		{
			name: "the column order",
			apply: func(m *model.Mapping) {
				m.Entities[0].Cols[0], m.Entities[0].Cols[1] = m.Entities[0].Cols[1], m.Entities[0].Cols[0]
			},
		},
		{
			name: "an enum label",
			apply: func(m *model.Mapping) {
				m.Entities[0].Cols[2].Column.Type.Labels = []string{"pending", "active", "banned"}
			},
		},
		{
			name:  "the enum label order",
			apply: func(m *model.Mapping) { m.Entities[0].Cols[2].Column.Type.Labels = []string{"active", "pending"} },
		},
		{
			name:  "which constraint backs a relation",
			apply: func(m *model.Mapping) { m.Entities[0].Rels[0].FK.Name = "posts_author_fkey" },
		},
		{
			name:  "a relation's direction",
			apply: func(m *model.Mapping) { m.Entities[0].Rels[0].FKSide = model.FKLocal },
		},
		{
			name:  "a relation's cardinality",
			apply: func(m *model.Mapping) { m.Entities[0].Rels[0].Cardinality = model.CardOne },
		},
		{
			// Whether the key is a Go field decides how the relation loads.
			name:  "whether a relation key is mapped",
			apply: func(m *model.Mapping) { m.Entities[0].Rels[0].KeyCols[0].FieldIdx = -1 },
		},
		{
			name:  "a relation's target",
			apply: func(m *model.Mapping) { m.Entities[0].Rels[0].Target = m.Entities[0] },
		},
		{
			name:  "losing a relation",
			apply: func(m *model.Mapping) { m.Entities[0].Rels = nil },
		},
		{
			name:  "losing an entity",
			apply: func(m *model.Mapping) { m.Entities = m.Entities[:1] },
		},
		{
			name:  "the table an entity maps to",
			apply: func(m *model.Mapping) { m.Entities[0].Table.Name = "accounts" },
		},
	}

	base := lock.Fingerprint(mapping())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mapping()
			tt.apply(m)
			if got := lock.Fingerprint(m); got == base {
				t.Errorf("the fingerprint did not change, so generated code would go on looking current")
			}
		})
	}
}

// These are facts about a particular database or a particular machine. Letting
// them into the fingerprint would report every colleague's checkout as stale.
func TestFingerprint_changesThatDoNot(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*model.Mapping)
	}{
		{
			name: "a type's OID",
			apply: func(m *model.Mapping) {
				m.Entities[0].Cols[2].Column.Type = &model.PGType{
					OID: 99999, Schema: "public", Name: "user_state", Kind: model.PGEnum,
					Labels: []string{"pending", "active"},
				}
			},
		},
		{
			name:  "the order columns were created in",
			apply: func(m *model.Mapping) { m.Entities[0].Cols[1].Column.AttNum = 42 },
		},
		{
			name: "a column nobody mapped",
			apply: func(m *model.Mapping) {
				m.Entities[0].Table.Cols = append(m.Entities[0].Table.Cols, &model.PGColumn{Name: "scratch", Type: textType()})
			},
		},
		{
			// Sorted, so a package reordered in the configuration is not a
			// change to the mapping.
			name:  "the order entities were scanned in",
			apply: func(m *model.Mapping) { m.Entities[0], m.Entities[1] = m.Entities[1], m.Entities[0] },
		},
		{
			name:  "where the package lives on disk",
			apply: func(m *model.Mapping) { m.Entities[0].Entity.PkgDir = "/somewhere/else" },
		},
		{
			name:  "the table's own OID",
			apply: func(m *model.Mapping) { m.Entities[0].Table.OID = 12345 },
		},
	}

	base := lock.Fingerprint(mapping())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mapping()
			tt.apply(m)
			if got := lock.Fingerprint(m); got != base {
				t.Errorf("the fingerprint changed, so every checkout would report as stale for no reason")
			}
		})
	}
}

func TestFile_roundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lock.Name)

	f, present, err := lock.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if present {
		t.Fatal("a lock was found in an empty directory")
	}
	if got := lock.Compare(f, present, "abc"); got != lock.Missing {
		t.Errorf("state = %d, want Missing", got)
	}

	if err := lock.Write(path, "abc"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f, present, err = lock.Read(path)
	if err != nil || !present {
		t.Fatalf("Read: %v, present = %v", err, present)
	}
	if got := lock.Compare(f, present, "abc"); got != lock.Current {
		t.Errorf("state = %d, want Current", got)
	}
	if got := lock.Compare(f, present, "def"); got != lock.Stale {
		t.Errorf("state = %d, want Stale", got)
	}

	// A lock from a future format is not something to compare against.
	f.Version = lock.Version + 1
	if got := lock.Compare(f, present, "abc"); got != lock.Unknown {
		t.Errorf("state = %d, want Unknown", got)
	}

	// The file is readable rather than a blob, so a diff of it says something.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !strings.Contains(string(data), `"mapping_sha256": "abc"`) {
		t.Errorf("lock file = %s, want the fingerprint to be legible", data)
	}
}

// A lock nobody can parse is a tool failure, not a silent fresh start: silently
// treating it as missing would let a corrupt file hide a stale generation.
func TestRead_malformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lock.Name)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if _, _, err := lock.Read(path); err == nil {
		t.Error("a malformed lock was accepted")
	}
}

func TestWrite_replacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lock.Name)
	if err := lock.Write(path, "first"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := lock.Write(path, "second"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	f, _, err := lock.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if f.Mapping != "second" {
		t.Errorf("fingerprint = %q, want the second write", f.Mapping)
	}
	// The temporary file it landed through is gone.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the directory: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d files, want only the lock", len(entries))
	}
}

// M12.2, stated directly: two instantiations of one generic origin do not share
// a fingerprint.
//
// This is the property Stage 1 of M12.2 existed to make true. Range[int32] and
// Range[int64] are both "github.com/AlexAli29/orm.Range" to go/types' object,
// and if the fingerprint were taken over that name the generator would happily
// leave an int8range column mapped to int32 across a change nobody noticed.
func TestFingerprint_genericArgumentsAreDistinguished(t *testing.T) {
	rangeOf := func(arg string, kind model.GoKind) model.GoType {
		return model.GoType{
			Src: "orm.Range[" + arg + "]", Value: "orm.Range[" + arg + "]",
			Named:    "github.com/AlexAli29/orm.Range",
			Kind:     model.KindStruct,
			TypeArgs: []model.GoType{{Src: arg, Value: arg, Kind: kind}},
		}
	}

	seen := map[string]string{}
	for _, tt := range []struct {
		name string
		typ  model.GoType
	}{
		{"Range[int32]", rangeOf("int32", model.KindInt32)},
		{"Range[int64]", rangeOf("int64", model.KindInt64)},
		{"Range[time.Time]", rangeOf("time.Time", model.KindTime)},
	} {
		m := mapping()
		m.Entities[0].Cols[1].Field.Type = tt.typ
		fp := lock.Fingerprint(m)
		if other, dup := seen[fp]; dup {
			t.Errorf("%s and %s share the mapping fingerprint %s", tt.name, other, fp)
		}
		seen[fp] = tt.name
	}
}

// The four states a lock can be in, told apart.
//
// Compare is one switch, and it is the whole of what `orm check --generated`
// decides. Each arm means something different to a user: no lock is a project
// that has not generated yet and is not a failure; a version this tool does not
// understand must not be guessed at; equal digests mean the committed code was
// produced from this mapping; and anything else means it was not.
//
// The last arm is the one worth pinning. Accepting a mismatch is silent — the
// command prints that the generated code is current and exits zero — and it is
// the exact failure the fingerprint exists to prevent, so a mistake there
// undoes every other property the lock has.
func TestCompare_statesAreDistinguished(t *testing.T) {
	const mine, theirs = "aaaa", "bbbb"
	present := &lock.File{Version: lock.Version, Mapping: mine}

	for _, c := range []struct {
		what        string
		file        *lock.File
		present     bool
		fingerprint string
		want        lock.State
	}{
		{"no lock file", nil, false, mine, lock.Missing},
		{"a format this tool does not understand",
			&lock.File{Version: lock.Version + 1, Mapping: mine}, true, mine, lock.Unknown},
		{"the digests match", present, true, mine, lock.Current},
		{"the digests differ", present, true, theirs, lock.Stale},
		{"the lock records no digest at all",
			&lock.File{Version: lock.Version}, true, mine, lock.Stale},
	} {
		t.Run(c.what, func(t *testing.T) {
			if got := lock.Compare(c.file, c.present, c.fingerprint); got != c.want {
				t.Errorf("Compare = %v, want %v. A generated-code check that answers this "+
					"wrongly is silent: it prints that the code is current and exits zero",
					got, c.want)
			}
		})
	}
}
