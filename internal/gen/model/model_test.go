package model_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/model"
)

func TestParseTableRef(t *testing.T) {
	tests := []struct {
		in      string
		want    model.TableRef
		wantErr string
	}{
		{in: "users", want: model.TableRef{Name: "users"}},
		{in: "analytics.events", want: model.TableRef{Schema: "analytics", Name: "events"}},
		{in: "", wantErr: "empty table name"},
		{in: ".events", wantErr: "malformed"},
		{in: "analytics.", wantErr: "malformed"},
		{in: "a.b.c", wantErr: "more than one qualifier"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := model.ParseTableRef(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseTableRef(%q) error = %v, want it to contain %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTableRef(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseTableRef(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
			if round := got.String(); round != tt.in {
				t.Errorf("String() = %q, want the reference as written, %q", round, tt.in)
			}
		})
	}
}

func TestPosition(t *testing.T) {
	tests := []struct {
		name string
		pos  model.Position
		want string
		zero bool
	}{
		{name: "unknown", pos: model.Position{}, want: "", zero: true},
		{name: "file only", pos: model.Position{File: "a.go"}, want: "a.go"},
		{name: "file and line", pos: model.Position{File: "a.go", Line: 3}, want: "a.go:3"},
		{name: "full", pos: model.Position{File: "a.go", Line: 3, Col: 2}, want: "a.go:3:2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pos.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if got := tt.pos.IsZero(); got != tt.zero {
				t.Errorf("IsZero() = %v, want %v", got, tt.zero)
			}
		})
	}
}

func TestGoType_nullable(t *testing.T) {
	tests := []struct {
		name string
		typ  model.GoType
		want bool
	}{
		{name: "plain string", typ: model.GoType{Kind: model.KindString}},
		{name: "pointer", typ: model.GoType{Kind: model.KindString, Ptr: true}, want: true},
		{name: "sql null", typ: model.GoType{Kind: model.KindString, SQLNull: true}, want: true},
		{name: "map", typ: model.GoType{Kind: model.KindMap}, want: true},
		{name: "any", typ: model.GoType{Kind: model.KindAny}, want: true},
		// A nil slice and an empty array are the same value on the wire, so a
		// slice has nowhere to put NULL.
		{name: "slice", typ: model.GoType{Kind: model.KindSlice}},
		{name: "bytes", typ: model.GoType{Kind: model.KindBytes}},
		{name: "pointer to slice", typ: model.GoType{Kind: model.KindSlice, Ptr: true}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.typ.Nullable(); got != tt.want {
				t.Errorf("Nullable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGoType_isEnum(t *testing.T) {
	named := model.GoType{Named: "app.Status", Kind: model.KindString}
	if named.IsEnum() {
		t.Error("a named type with no constants is not an enum")
	}
	named.Enum = []model.EnumConst{{Name: "StatusA", Str: "a", IsString: true}}
	if !named.IsEnum() {
		t.Error("a named type with constants is an enum")
	}

	unnamed := model.GoType{Kind: model.KindString, Enum: named.Enum}
	if unnamed.IsEnum() {
		t.Error("an unnamed type cannot be an enum, whatever constants were found")
	}
}

func TestEnumConst_value(t *testing.T) {
	if got := (model.EnumConst{Str: "pending", IsString: true}).Value(); got != "pending" {
		t.Errorf("string constant Value() = %q", got)
	}
	if got := (model.EnumConst{Int: 7}).Value(); got != "7" {
		t.Errorf("integer constant Value() = %q", got)
	}
}

// schema builds a small schema by hand, so that the model's own logic can be
// tested without a database.
func schema() (*model.Schema, *model.PGTable) {
	text := &model.PGType{Name: "text", Schema: "pg_catalog"}
	tight := &model.PGType{Name: "tight", Schema: "public", Kind: model.PGDomain, Elem: text, DomainNotNull: true}
	loose := &model.PGType{Name: "loose", Schema: "public", Kind: model.PGDomain, Elem: text}
	int8t := &model.PGType{Name: "int8", Schema: "pg_catalog"}

	users := &model.PGTable{Schema: "public", Name: "users", Kind: 'r'}
	cols := []*model.PGColumn{
		{Table: users, Name: "id", AttNum: 1, Type: int8t, NotNull: true, Identity: 'd'},
		{Table: users, Name: "name", AttNum: 2, Type: text, NotNull: true},
		{Table: users, Name: "nickname", AttNum: 3, Type: text},
		{Table: users, Name: "created_at", AttNum: 4, Type: text, NotNull: true, HasDefault: true},
		{Table: users, Name: "slug", AttNum: 5, Type: text, Generated: 's'},
		{Table: users, Name: "strict_domain", AttNum: 6, Type: tight},
		{Table: users, Name: "loose_domain", AttNum: 7, Type: loose},
		{Table: users, Name: "always", AttNum: 8, Type: int8t, NotNull: true, Identity: 'a'},
	}
	users.Cols = cols
	users.PK = cols[:1]

	other := &model.PGTable{Schema: "analytics", Name: "users", Kind: 'r'}
	events := &model.PGTable{Schema: "analytics", Name: "events", Kind: 'r'}
	return model.NewSchema([]string{"public", "analytics"}, []*model.PGTable{events, other, users}), users
}

func TestPGColumn_facts(t *testing.T) {
	_, users := schema()
	tests := []struct {
		col                                        string
		nullable, suppliable, writable, isIdentity bool
		isGenerated                                bool
	}{
		{col: "id", suppliable: true, writable: true, isIdentity: true},
		{col: "name", suppliable: false, writable: true},
		{col: "nickname", nullable: true, suppliable: true, writable: true},
		{col: "created_at", suppliable: true, writable: true},
		{col: "slug", nullable: true, suppliable: true, isGenerated: true},
		// A NOT NULL domain makes the column non-nullable even though the
		// column itself carries no NOT NULL.
		{col: "strict_domain", nullable: false, suppliable: false, writable: true},
		{col: "loose_domain", nullable: true, suppliable: true, writable: true},
		// GENERATED ALWAYS AS IDENTITY cannot be written to at all.
		{col: "always", suppliable: true, isIdentity: true},
	}
	for _, tt := range tests {
		t.Run(tt.col, func(t *testing.T) {
			c := users.Column(tt.col)
			if c == nil {
				t.Fatalf("no column %s", tt.col)
			}
			if got := c.Nullable(); got != tt.nullable {
				t.Errorf("Nullable() = %v, want %v", got, tt.nullable)
			}
			if got := c.Suppliable(); got != tt.suppliable {
				t.Errorf("Suppliable() = %v, want %v", got, tt.suppliable)
			}
			if got := c.Writable(); got != tt.writable {
				t.Errorf("Writable() = %v, want %v", got, tt.writable)
			}
			if got := c.IsIdentity(); got != tt.isIdentity {
				t.Errorf("IsIdentity() = %v, want %v", got, tt.isIdentity)
			}
			if got := c.IsGenerated(); got != tt.isGenerated {
				t.Errorf("IsGenerated() = %v, want %v", got, tt.isGenerated)
			}
		})
	}

	if got := users.Column("id").Qualified(); got != "public.users.id" {
		t.Errorf("Qualified() = %q", got)
	}
	if users.Column("nope") != nil {
		t.Error("Column returned a column that does not exist")
	}
}

func TestSchema_lookupFollowsTheSearchPath(t *testing.T) {
	s, _ := schema()

	// public comes first in the search path, so a bare name resolves there.
	got, ok := s.Lookup(model.TableRef{Name: "users"})
	if !ok || got.Schema != "public" {
		t.Errorf("unqualified users resolved to %v, want public.users", got)
	}
	// A name only the second schema has still resolves.
	got, ok = s.Lookup(model.TableRef{Name: "events"})
	if !ok || got.Schema != "analytics" {
		t.Errorf("unqualified events resolved to %v, want analytics.events", got)
	}
	// A qualified reference ignores the search path.
	got, ok = s.Lookup(model.TableRef{Schema: "analytics", Name: "users"})
	if !ok || got.Schema != "analytics" {
		t.Errorf("analytics.users resolved to %v", got)
	}
	if _, ok := s.Lookup(model.TableRef{Name: "ghosts"}); ok {
		t.Error("a table that does not exist resolved")
	}
	if _, ok := s.Lookup(model.TableRef{Schema: "other", Name: "users"}); ok {
		t.Error("a schema outside the search path resolved")
	}

	var nilSchema *model.Schema
	if _, ok := nilSchema.Lookup(model.TableRef{Name: "users"}); ok {
		t.Error("Lookup on a nil schema reported success")
	}
}

func TestPGType_stringAndResolve(t *testing.T) {
	text := &model.PGType{Name: "text", Schema: "pg_catalog"}
	domain := &model.PGType{Name: "email", Schema: "public", Kind: model.PGDomain, Elem: text}
	nested := &model.PGType{Name: "work_email", Schema: "public", Kind: model.PGDomain, Elem: domain}
	array := &model.PGType{Name: "_text", Schema: "pg_catalog", Kind: model.PGArray, Elem: text}

	if got := text.String(); got != "text" {
		t.Errorf("a pg_catalog type renders as %q, want unqualified", got)
	}
	if got := domain.String(); got != "public.email" {
		t.Errorf("a user type renders as %q, want qualified", got)
	}
	if got := array.String(); got != "text[]" {
		t.Errorf("an array renders as %q", got)
	}
	if got := nested.Resolve(); got != text {
		t.Errorf("Resolve() followed nested domains to %v, want text", got)
	}
	if got := array.Resolve(); got != array {
		t.Error("Resolve() unwrapped an array; an array is a distinct shape")
	}
	var nilType *model.PGType
	if got := nilType.String(); got != "<unknown>" {
		t.Errorf("a nil type renders as %q", got)
	}
	if nilType.Resolve() != nil {
		t.Error("Resolve() on a nil type returned something")
	}
}

func TestPGUnique_total(t *testing.T) {
	col := &model.PGColumn{Name: "user_id"}
	tests := []struct {
		name string
		u    model.PGUnique
		want bool
	}{
		{name: "plain", u: model.PGUnique{Cols: []*model.PGColumn{col}}, want: true},
		{name: "primary key", u: model.PGUnique{Cols: []*model.PGColumn{col}, Primary: true}, want: true},
		{name: "partial", u: model.PGUnique{Cols: []*model.PGColumn{col}, Partial: true}},
		{name: "expression", u: model.PGUnique{Cols: []*model.PGColumn{col}, Expression: true}},
		{name: "no columns", u: model.PGUnique{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.u.Total(); got != tt.want {
				t.Errorf("Total() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRelKeyCol_mapped(t *testing.T) {
	col := &model.PGColumn{Name: "author_id"}
	if !(model.RelKeyCol{Column: col, FieldIdx: 0}).Mapped() {
		t.Error("field index 0 is a mapped field")
	}
	// -1 is a legitimate state, not an error: the column is the source of
	// truth and a loader can always select it.
	if (model.RelKeyCol{Column: col, FieldIdx: -1}).Mapped() {
		t.Error("field index -1 reported as mapped")
	}
}

func TestEntityMapping_colIdx(t *testing.T) {
	a := &model.PGColumn{Name: "a"}
	b := &model.PGColumn{Name: "b"}
	missing := &model.PGColumn{Name: "c"}
	em := &model.EntityMapping{Cols: []model.ColMapping{{Column: a}, {Column: b}}}

	if got := em.ColIdx(b); got != 1 {
		t.Errorf("ColIdx(b) = %d, want 1", got)
	}
	if got := em.ColIdx(missing); got != -1 {
		t.Errorf("ColIdx(unmapped) = %d, want -1", got)
	}
}

func TestCardinalityAndFKSide(t *testing.T) {
	// Cardinality and FKSide are separate on purpose: one is a property of the
	// Go field, the other of the schema.
	if got := model.CardOne.String(); got != "one" {
		t.Errorf("CardOne = %q", got)
	}
	if got := model.CardMany.String(); got != "many" {
		t.Errorf("CardMany = %q", got)
	}
	if got := model.FKLocal.String(); got != "local" {
		t.Errorf("FKLocal = %q", got)
	}
	if got := model.FKRemote.String(); got != "remote" {
		t.Errorf("FKRemote = %q", got)
	}

	for in, want := range map[string]model.FKSide{"local": model.FKLocal, "remote": model.FKRemote} {
		got, err := model.ParseFKSide(in)
		if err != nil {
			t.Errorf("ParseFKSide(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFKSide(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := model.ParseFKSide("sideways"); err == nil {
		t.Error("ParseFKSide(\"sideways\") succeeded, want an error")
	}
}

func TestGoRel_carriesNoDirection(t *testing.T) {
	// The relation model deliberately has no side field. If one is ever added,
	// direction stops being derived from the catalog and starts being declared,
	// which is the thing this design refuses to do.
	rel := model.GoRel{Cardinality: model.CardMany, Target: "app.Post"}
	if rel.Cardinality != model.CardMany || rel.Target != "app.Post" {
		t.Fatalf("GoRel = %+v", rel)
	}
	if got := (model.GoRel{}); got.Cardinality != model.CardOne {
		t.Errorf("the zero GoRel has cardinality %v, want one", got.Cardinality)
	}
}

func TestGoEntity_names(t *testing.T) {
	e := &model.GoEntity{Name: "User", PkgPath: "example.com/app/internal/domain", PkgName: "domain"}
	if got := e.Qualified(); got != "example.com/app/internal/domain.User" {
		t.Errorf("Qualified() = %q", got)
	}
	if got := e.Display(); got != "domain.User" {
		t.Errorf("Display() = %q", got)
	}
	bare := &model.GoEntity{Name: "User"}
	if got := bare.Display(); got != "User" {
		t.Errorf("Display() with no package = %q", got)
	}
}

func TestGoField_isRelation(t *testing.T) {
	scalar := model.GoField{Name: "Title"}
	if scalar.IsRelation() {
		t.Error("a field with no GoRel is not a relation")
	}
	rel := model.GoField{Name: "Author", Rel: &model.GoRel{}}
	if !rel.IsRelation() {
		t.Error("a field with a GoRel is a relation")
	}
}

func TestGoKind_names(t *testing.T) {
	if got := model.KindBytes.String(); got != "[]byte" {
		t.Errorf("KindBytes = %q", got)
	}
	if got := model.GoKind(200).String(); got != "unknown" {
		t.Errorf("an unregistered kind renders as %q", got)
	}
	for _, k := range []model.GoKind{model.KindInt, model.KindInt64, model.KindUint8, model.KindUint64} {
		if !k.IsInteger() {
			t.Errorf("%v is an integer kind", k)
		}
	}
	for _, k := range []model.GoKind{model.KindString, model.KindFloat64, model.KindBool, model.KindTime} {
		if k.IsInteger() {
			t.Errorf("%v is not an integer kind", k)
		}
	}
}

func TestPGTypeKind_names(t *testing.T) {
	if got := model.PGEnum.String(); got != "enum" {
		t.Errorf("PGEnum = %q", got)
	}
	if got := model.PGTypeKind(200).String(); got != "unknown" {
		t.Errorf("an unregistered kind renders as %q", got)
	}
}
