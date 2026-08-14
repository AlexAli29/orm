package emit_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/emit"
	"github.com/AlexAli29/orm/internal/gen/model"
)

// The emitter discovers nothing, so its tests build a mapping by hand. That is
// the point of the split: capability assignment can be exercised over every
// PostgreSQL type without a database, a schema, or a Go package to parse.

const pkgPath = "example.com/app/internal/domain"

var (
	tText        = &model.PGType{Name: "text", Schema: "pg_catalog"}
	tVarchar     = &model.PGType{Name: "varchar", Schema: "pg_catalog"}
	tCitext      = &model.PGType{Name: "citext", Schema: "public"}
	tInt4        = &model.PGType{Name: "int4", Schema: "pg_catalog"}
	tInt8        = &model.PGType{Name: "int8", Schema: "pg_catalog"}
	tFloat8      = &model.PGType{Name: "float8", Schema: "pg_catalog"}
	tBool        = &model.PGType{Name: "bool", Schema: "pg_catalog"}
	tJSONB       = &model.PGType{Name: "jsonb", Schema: "pg_catalog"}
	tBytea       = &model.PGType{Name: "bytea", Schema: "pg_catalog"}
	tNumeric     = &model.PGType{Name: "numeric", Schema: "pg_catalog"}
	tUUID        = &model.PGType{Name: "uuid", Schema: "pg_catalog"}
	tTimestamptz = &model.PGType{Name: "timestamptz", Schema: "pg_catalog"}
	tTextArray   = &model.PGType{Name: "_text", Schema: "pg_catalog", Kind: model.PGArray, Elem: tText}
	tEnum        = &model.PGType{Name: "user_state", Schema: "public", Kind: model.PGEnum, Labels: []string{"a", "b"}}
	tDomain      = &model.PGType{Name: "email", Schema: "public", Kind: model.PGDomain, Elem: tText}
)

// col describes one column and the Go field opposite it.
type col struct {
	field    string
	goType   string
	value    string
	refs     []string
	column   string
	pgType   *model.PGType
	nullable bool
}

// mapping assembles one entity into a Mapping the emitter can render.
func mapping(entity, table string, cols ...col) *model.Mapping {
	t := &model.PGTable{Schema: "public", Name: table, Kind: 'r'}
	e := &model.GoEntity{
		Name:    entity,
		PkgPath: pkgPath,
		PkgName: "domain",
		PkgDir:  "/app/internal/domain",
		Table:   model.TableRef{Schema: "public", Name: table},
	}
	em := &model.EntityMapping{Entity: e, Table: t}

	for i, c := range cols {
		value := c.value
		if value == "" {
			value = c.goType
		}
		e.Fields = append(e.Fields, model.GoField{
			Name: c.field,
			Type: model.GoType{Src: c.goType, Value: value, Refs: c.refs, Ptr: c.nullable},
		})
		pc := &model.PGColumn{
			Table: t, Name: c.column, AttNum: i + 1, Type: c.pgType, NotNull: !c.nullable,
		}
		t.Cols = append(t.Cols, pc)
		em.Cols = append(em.Cols, model.ColMapping{Field: &e.Fields[i], Idx: i, Column: pc})
	}
	t.PK = t.Cols[:1]
	return &model.Mapping{Entities: []*model.EntityMapping{em}}
}

// spaces collapses runs of whitespace, so that an assertion about generated
// code is about the code rather than about how gofmt chose to align it — which
// changes whenever a neighbouring field's name gets longer.
var spaces = regexp.MustCompile(`[ \t]+`)

func hasCode(haystack, needle string) bool {
	return strings.Contains(spaces.ReplaceAllString(haystack, " "), spaces.ReplaceAllString(needle, " "))
}

func generate(t *testing.T, m *model.Mapping) map[string]string {
	t.Helper()
	files, err := emit.Generate(emit.Input{Mapping: m})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[filepath.Base(f.Path)] = string(f.Content)
	}
	return out
}

func TestGenerate_writeMetadata(t *testing.T) {
	// The write path decides what an insert may say from these flags, so a
	// column that is generated, identity or defaulted has to be recorded as
	// such rather than discovered from an error at execution time.
	m := mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "Email", goType: "string", column: "email", pgType: tText},
		col{field: "Nickname", goType: "*string", value: "string", column: "nickname", pgType: tText, nullable: true},
	)
	cols := m.Entities[0].Table.Cols
	cols[0].Identity = 'd'
	cols[1].HasDefault = true
	cols[2].Generated = 's'

	got := generate(t, m)["orm_meta.gen.go"]
	for _, want := range []string{
		`{Name: "id", Field: "ID", NotNull: true, Identity: true},`,
		`{Name: "email", Field: "Email", NotNull: true, HasDefault: true},`,
		`{Name: "nickname", Field: "Nickname", Generated: true},`,
	} {
		if !hasCode(got, want) {
			t.Errorf("orm_meta.gen.go does not contain %q:\n%s", want, got)
		}
	}
}

func TestGenerate_capabilityComesFromPostgreSQL(t *testing.T) {
	tests := []struct {
		name string
		col  col
		want string
	}{
		{
			name: "text NOT NULL",
			col:  col{field: "Email", goType: "string", column: "email", pgType: tText},
			want: "Email orm.TextCol[User]",
		},
		{
			name: "text NULL",
			col:  col{field: "Nickname", goType: "*string", value: "string", column: "nickname", pgType: tText, nullable: true},
			want: "Nickname orm.NullTextCol[User]",
		},
		{
			name: "varchar is text too",
			col:  col{field: "Code", goType: "string", column: "code", pgType: tVarchar},
			want: "Code orm.TextCol[User]",
		},
		{
			name: "citext is text too",
			col:  col{field: "Handle", goType: "string", column: "handle", pgType: tCitext},
			want: "Handle orm.TextCol[User]",
		},
		{
			name: "a domain reconciles through its base type",
			col:  col{field: "Mail", goType: "string", column: "mail", pgType: tDomain},
			want: "Mail orm.TextCol[User]",
		},
		{
			name: "int8 NOT NULL",
			col:  col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
			want: "ID orm.OrdCol[User, int64]",
		},
		{
			name: "int4 NOT NULL",
			col:  col{field: "Age", goType: "int32", column: "age", pgType: tInt4},
			want: "Age orm.OrdCol[User, int32]",
		},
		{
			name: "float8 NULL",
			col:  col{field: "Score", goType: "*float64", value: "float64", column: "score", pgType: tFloat8, nullable: true},
			want: "Score orm.NullOrdCol[User, float64]",
		},
		{
			name: "timestamptz NOT NULL",
			col:  col{field: "CreatedAt", goType: "time.Time", value: "time.Time", refs: []string{"time"}, column: "created_at", pgType: tTimestamptz},
			want: "CreatedAt orm.OrdCol[User, time.Time]",
		},
		{
			name: "numeric is ordered once configured",
			col:  col{field: "Amount", goType: "Decimal", column: "amount", pgType: tNumeric},
			want: "Amount orm.OrdCol[User, Decimal]",
		},
		{
			name: "uuid is ordered",
			col:  col{field: "Ref", goType: "UUID", column: "ref", pgType: tUUID},
			want: "Ref orm.OrdCol[User, UUID]",
		},
		{
			name: "an enum is ordered but not text",
			col:  col{field: "State", goType: "UserState", column: "state", pgType: tEnum},
			want: "State orm.OrdCol[User, UserState]",
		},
		{
			name: "bool is equality only",
			col:  col{field: "Active", goType: "bool", column: "active", pgType: tBool},
			want: "Active orm.Col[User, bool]",
		},
		{
			// jsonb has an order defined for indexing, and exposing it would
			// invite the reading that it compares documents the way a person
			// would.
			name: "jsonb does not become ordered",
			col:  col{field: "Profile", goType: "map[string]any", column: "profile", pgType: tJSONB},
			want: "Profile orm.Col[User, map[string]any]",
		},
		{
			name: "bytea is equality only",
			col:  col{field: "Avatar", goType: "[]byte", column: "avatar", pgType: tBytea},
			want: "Avatar orm.Col[User, []byte]",
		},
		{
			name: "an array is equality only",
			col:  col{field: "Tags", goType: "[]string", column: "tags", pgType: tTextArray},
			want: "Tags orm.Col[User, []string]",
		},
		{
			name: "nullable bool",
			col:  col{field: "Flag", goType: "*bool", value: "bool", column: "flag", pgType: tBool, nullable: true},
			want: "Flag orm.NullCol[User, bool]",
		},
		{
			// Like takes a pattern, and a pattern is a string. A named string
			// type keeps everything else PostgreSQL offers.
			name: "a named string type on a text column stays ordered",
			col:  col{field: "Slug", goType: "Slug", column: "slug", pgType: tText},
			want: "Slug orm.OrdCol[User, Slug]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := generate(t, mapping("User", "users", tt.col))
			if !hasCode(files["orm_tables.gen.go"], tt.want) {
				t.Errorf("generated descriptor does not contain %q:\n%s", tt.want, files["orm_tables.gen.go"])
			}
		})
	}
}

func TestGenerate_tables(t *testing.T) {
	files := generate(t, mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "Email", goType: "string", column: "email", pgType: tText},
	))
	const want = `// Code generated by orm. DO NOT EDIT.

package domain

import (
	"github.com/AlexAli29/orm"
)

// usersSource is the occurrence of public.users that Users reads from.
var usersSource = orm.NewSource("public", "users")

// userTable holds one typed descriptor per mapped column of public.users.
type userTable struct {
	src   *orm.Source
	ID    orm.OrdCol[User, int64]
	Email orm.TextCol[User]
}

// newUserTable builds the descriptors for one occurrence of public.users.
func newUserTable(src *orm.Source) userTable {
	return userTable{
		src:   src,
		ID:    orm.NewOrdCol[User, int64](src, "id"),
		Email: orm.NewTextCol[User](src, "email"),
	}
}

// Users addresses the columns of public.users.
var Users = newUserTable(usersSource)

// As returns descriptors for a second occurrence of public.users under
// alias, leaving Users untouched. Use it with QueryFrom, or to compare
// one occurrence of the table against another.
func (t userTable) As(alias string) userTable { return newUserTable(t.src.As(alias)) }

// Source returns the occurrence these descriptors qualify against.
func (t userTable) Source() *orm.Source { return t.src }
`
	if got := files["orm_tables.gen.go"]; got != want {
		t.Errorf("orm_tables.gen.go =\n%s\nwant\n%s", got, want)
	}
}

func TestGenerate_metaAndScanner(t *testing.T) {
	files := generate(t, mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "Email", goType: "string", column: "email", pgType: tText},
	))
	got := files["orm_meta.gen.go"]

	for _, want := range []string{
		`var userMeta = orm.EntityMeta[User]{`,
		`Table: orm.TableID{Schema: "public", Name: "users"},`,
		// The repository has to hold the same occurrence the descriptors were
		// built from, or every predicate over them would be out of scope.
		`Source: usersSource,`,
		`{Name: "id", Field: "ID", NotNull: true},`,
		`{Name: "email", Field: "Email", NotNull: true},`,
		`Dest:  userDest,`,
		`Value: userValue,`,
		`func userDest(e *User, idx int) any {`,
		"case 0:\n\t\treturn &e.ID",
		"case 1:\n\t\treturn &e.Email",
		"default:\n\t\treturn nil",
		// The value accessor is the scanner's counterpart, written the same
		// way so that an insert reads the entity without reflection either.
		`func userValue(e *User, idx int) any {`,
		"case 0:\n\t\treturn e.ID",
		"case 1:\n\t\treturn e.Email",
	} {
		if !hasCode(got, want) {
			t.Errorf("orm_meta.gen.go does not contain %q:\n%s", want, got)
		}
	}
	// The read path must not acquire reflection by accident. The prose above
	// says "reflection", so the check looks for the package rather than the
	// word.
	for _, bad := range []string{`"reflect"`, "reflect."} {
		if strings.Contains(got, bad) {
			t.Errorf("the generated scanner uses %s:\n%s", bad, got)
		}
	}
}

func TestGenerate_scannerOrderMatchesTheColumnOrder(t *testing.T) {
	// The SELECT list and the scanner index into the same list. If they ever
	// disagreed, every row would be scanned into the wrong fields, and the
	// types would often still line up.
	files := generate(t, mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "Email", goType: "string", column: "email", pgType: tText},
		col{field: "Age", goType: "int32", column: "age", pgType: tInt4},
	))
	meta := files["orm_meta.gen.go"]

	columns := indexOfAll(meta, []string{`{Name: "id"`, `{Name: "email"`, `{Name: "age"`})
	scanner := indexOfAll(meta, []string{"return &e.ID", "return &e.Email", "return &e.Age"})
	for i := 1; i < len(columns); i++ {
		if columns[i] < columns[i-1] || scanner[i] < scanner[i-1] {
			t.Fatalf("the column list and the scanner disagree on order:\n%s", meta)
		}
	}
}

func indexOfAll(s string, subs []string) []int {
	out := make([]int, len(subs))
	for i, sub := range subs {
		out[i] = strings.Index(s, sub)
	}
	return out
}

func TestGenerate_db(t *testing.T) {
	files := generate(t, mapping("User", "users", col{field: "ID", goType: "int64", column: "id", pgType: tInt8}))
	got := files["orm_db.gen.go"]
	for _, want := range []string{
		"type DB struct {",
		"Users *orm.Repo[User]",
		"func New(ex orm.Executor) *DB {",
		"Users: orm.NewRepo(ex, &userMeta),",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("orm_db.gen.go does not contain %q:\n%s", want, got)
		}
	}
}

func TestGenerate_everyFileCarriesTheHeaderAndNoTimestamp(t *testing.T) {
	files := generate(t, mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "CreatedAt", goType: "time.Time", refs: []string{"time"}, column: "created_at", pgType: tTimestamptz},
	))
	if len(files) != 3 {
		t.Fatalf("generated %d files, want 3", len(files))
	}
	for name, content := range files {
		if !strings.HasPrefix(content, emit.Header) {
			t.Errorf("%s does not start with the generated-code header", name)
		}
		// A timestamp or a version would make every regeneration a diff and
		// every diff meaningless.
		for _, bad := range []string{"20", "generated at", "version"} {
			if strings.Contains(strings.ToLower(content), strings.ToLower(bad)) && bad != "20" {
				t.Errorf("%s contains %q", name, bad)
			}
		}
	}
}

func TestGenerate_importsAreGroupedAndMinimal(t *testing.T) {
	files := generate(t, mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "CreatedAt", goType: "time.Time", refs: []string{"time"}, column: "created_at", pgType: tTimestamptz},
	))
	tables := files["orm_tables.gen.go"]
	const want = "import (\n\t\"time\"\n\n\t\"github.com/AlexAli29/orm\"\n)"
	if !strings.Contains(tables, want) {
		t.Errorf("imports are not grouped the way gofmt writes them:\n%s", tables)
	}
	// The metadata file names no Go type, so it needs nothing but the runtime.
	if strings.Contains(files["orm_meta.gen.go"], `"time"`) {
		t.Errorf("orm_meta.gen.go imports time, which it does not use:\n%s", files["orm_meta.gen.go"])
	}
}

func TestGenerate_neverImportsThePackageItGeneratesInto(t *testing.T) {
	// An entity's own enum type is the ordinary reason a value type names a
	// package, and importing yourself does not compile.
	files := generate(t, mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "State", goType: "UserState", refs: []string{pkgPath}, column: "state", pgType: tEnum},
	))
	for name, content := range files {
		if strings.Contains(content, pkgPath) {
			t.Errorf("%s imports the package it is generated into:\n%s", name, content)
		}
	}
}

func TestGenerate_isDeterministic(t *testing.T) {
	build := func() map[string]string {
		return generate(t, mapping("User", "users",
			col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
			col{field: "State", goType: "UserState", column: "state", pgType: tEnum},
			col{field: "CreatedAt", goType: "time.Time", refs: []string{"time"}, column: "created_at", pgType: tTimestamptz},
		))
	}
	first := build()
	for run := range 3 {
		got := build()
		for name, content := range first {
			if got[name] != content {
				t.Fatalf("run %d produced different %s", run+2, name)
			}
		}
	}
}

func TestGenerate_entitiesComeOutInAFixedOrder(t *testing.T) {
	// Two entities declared in one order must generate in another, sorted one,
	// so that reordering a source file is not a diff in generated output.
	m := mapping("User", "users", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	second := mapping("Account", "accounts", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	m.Entities = append(m.Entities, second.Entities...)

	files := generate(t, m)
	tables := files["orm_tables.gen.go"]
	if strings.Index(tables, "accountTable") > strings.Index(tables, "userTable") {
		t.Errorf("entities are not sorted:\n%s", tables)
	}
}

func TestGenerate_reservedIdentifiers(t *testing.T) {
	tests := []struct {
		name     string
		reserved []string
		want     string
	}{
		{name: "the DB type", reserved: []string{"DB"}, want: "would redeclare DB"},
		{name: "the constructor", reserved: []string{"New"}, want: "would redeclare New"},
		{name: "the descriptor", reserved: []string{"Users"}, want: "would redeclare Users"},
		{name: "the source", reserved: []string{"usersSource"}, want: "would redeclare usersSource"},
		{name: "the metadata", reserved: []string{"userMeta"}, want: "would redeclare userMeta"},
		{name: "the scanner", reserved: []string{"userDest"}, want: "would redeclare userDest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mapping("User", "users", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
			_, err := emit.Generate(emit.Input{Mapping: m, Reserved: map[string][]string{pkgPath: tt.reserved}})
			if err == nil {
				t.Fatal("Generate succeeded over an identifier the package already declares")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			// Renaming would leave the author reading documentation about an
			// identifier that does not exist.
			if strings.Contains(err.Error(), "renamed") {
				t.Errorf("error = %v, suggesting a rename", err)
			}
		})
	}
}

func TestGenerate_identifiersThatAreNotIdentifiers(t *testing.T) {
	tests := []struct {
		name  string
		table string
		want  string
	}{
		{name: "a leading digit", table: "2fa_tokens", want: "not a Go identifier"},
		{name: "punctuation camel does not strip", table: "a.b", want: "not a Go identifier"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := mapping("Token", tt.table, col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
			_, err := emit.Generate(emit.Input{Mapping: m})
			if err == nil {
				t.Fatalf("Generate succeeded for table %q", tt.table)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestGenerate_snakeCaseTablesBecomeCamelCase(t *testing.T) {
	files := generate(t, mapping("Address", "billing_addresses", col{field: "ID", goType: "int64", column: "id", pgType: tInt8}))
	tables := files["orm_tables.gen.go"]
	for _, want := range []string{"var BillingAddresses = newAddressTable(billingAddressesSource)", "var billingAddressesSource ="} {
		if !strings.Contains(tables, want) {
			t.Errorf("generated code does not contain %q:\n%s", want, tables)
		}
	}
}

func TestGenerate_crossPackageOutputIsRefused(t *testing.T) {
	m := mapping("User", "users", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	m.Entities[0].Entity.OutputDir = "/app/internal/generated"

	_, err := emit.Generate(emit.Input{Mapping: m})
	if err == nil {
		t.Fatal("Generate succeeded writing outside the entity's package")
	}
	if !strings.Contains(err.Error(), "output: same") {
		t.Errorf("error = %v, want it to name the setting that fixes it", err)
	}
}

func TestGenerate_nothingToGenerate(t *testing.T) {
	for _, m := range []*model.Mapping{nil, {}} {
		if _, err := emit.Generate(emit.Input{Mapping: m}); err == nil {
			t.Errorf("Generate succeeded over %#v", m)
		}
	}
}

func TestIsGenerated(t *testing.T) {
	for _, name := range emit.GeneratedFiles {
		if !emit.IsGenerated(name) {
			t.Errorf("%s is generated but was not recognised", name)
		}
		if !emit.IsGenerated(filepath.Join("/a/b", name)) {
			t.Errorf("%s was not recognised through a path", name)
		}
	}
	for _, name := range []string{"entities.go", "user.go", "orm_tables.go", ""} {
		if emit.IsGenerated(name) {
			t.Errorf("%s is hand-written but was taken for output", name)
		}
	}
}

func TestGenerate_sqlNullFieldsGetNullableDescriptors(t *testing.T) {
	// database/sql's null wrappers carry NULL just as a pointer does, and the
	// value type behind them is what a predicate compares against.
	tests := []struct {
		name string
		col  col
		want string
	}{
		// The scanner reports the value type behind the wrapper, and the
		// packages that value type names — which for these two is none at
		// all, since string and int64 are builtins. The descriptor names the
		// value, never the wrapper, so database/sql is not imported either.
		{
			name: "sql.Null[string] on a nullable text column",
			col:  col{field: "Bio", goType: "sql.Null[string]", value: "string", column: "bio", pgType: tText, nullable: true},
			want: "Bio orm.NullTextCol[User]",
		},
		{
			name: "sql.NullInt64 on a nullable int8 column",
			col:  col{field: "Count", goType: "sql.NullInt64", value: "int64", column: "count", pgType: tInt8, nullable: true},
			want: "Count orm.NullOrdCol[User, int64]",
		},
		{
			name: "sql.Null[time.Time] keeps the package its value names",
			col:  col{field: "Seen", goType: "sql.Null[time.Time]", value: "time.Time", refs: []string{"time"}, column: "seen", pgType: tTimestamptz, nullable: true},
			want: "Seen orm.NullOrdCol[User, time.Time]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := generate(t, mapping("User", "users", tt.col))
			if !hasCode(files["orm_tables.gen.go"], tt.want) {
				t.Errorf("generated descriptor does not contain %q:\n%s", tt.want, files["orm_tables.gen.go"])
			}
			if strings.Contains(files["orm_tables.gen.go"], `"database/sql"`) {
				t.Errorf("orm_tables.gen.go imports database/sql, which it does not name:\n%s", files["orm_tables.gen.go"])
			}
		})
	}
}

func TestGenerate_twoEntitiesWhoseNamesCollide(t *testing.T) {
	// Different table names that camel-case to one identifier would silently
	// give two entities the same descriptor.
	m := mapping("Log", "user_data", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	other := mapping("Event", "userData", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	m.Entities = append(m.Entities, other.Entities...)

	_, err := emit.Generate(emit.Input{Mapping: m})
	if err == nil {
		t.Fatal("Generate succeeded over two tables that produce one identifier")
	}
	if !strings.Contains(err.Error(), "both generate UserData") {
		t.Errorf("error = %v, want it to name the identifier and both sources", err)
	}
}

func TestGenerate_twoEntitiesOfTheSameNameInOnePackage(t *testing.T) {
	// This cannot arise from Go source, where a package declares each name
	// once, but the guard is what keeps the private naming policy sound.
	m := mapping("User", "users", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	other := mapping("User", "accounts", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	m.Entities = append(m.Entities, other.Entities...)

	_, err := emit.Generate(emit.Input{Mapping: m})
	if err == nil {
		t.Fatal("Generate succeeded over two entities of one name")
	}
	if !strings.Contains(err.Error(), "userTable") {
		t.Errorf("error = %v, want it to name the colliding identifier", err)
	}
}

func TestGenerate_aPackageWithNoDirectory(t *testing.T) {
	m := mapping("User", "users", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	m.Entities[0].Entity.PkgDir = ""

	_, err := emit.Generate(emit.Input{Mapping: m})
	if err == nil {
		t.Fatal("Generate succeeded with nowhere to write")
	}
	if !strings.Contains(err.Error(), "no directory") {
		t.Errorf("error = %v", err)
	}
}

func TestGenerate_severalPackagesEachGetTheirOwnFiles(t *testing.T) {
	m := mapping("User", "users", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	other := mapping("Order", "orders", col{field: "ID", goType: "int64", column: "id", pgType: tInt8})
	other.Entities[0].Entity.PkgPath = "example.com/app/internal/billing"
	other.Entities[0].Entity.PkgName = "billing"
	other.Entities[0].Entity.PkgDir = "/app/internal/billing"
	m.Entities = append(m.Entities, other.Entities...)

	files, err := emit.Generate(emit.Input{Mapping: m})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(files) != 6 {
		t.Fatalf("generated %d files, want 3 per package", len(files))
	}
	// Each package gets its own DB, named the same in each, because they are
	// different packages.
	var dbs int
	for _, f := range files {
		if filepath.Base(f.Path) == "orm_db.gen.go" {
			dbs++
			if !strings.Contains(string(f.Content), "type DB struct {") {
				t.Errorf("%s declares no DB", f.Path)
			}
		}
	}
	if dbs != 2 {
		t.Errorf("generated %d DB files, want one per package", dbs)
	}
	// Packages come out in a fixed order, so the file list is stable.
	if !strings.Contains(files[0].Path, "billing") {
		t.Errorf("packages are not sorted: first file is %s", files[0].Path)
	}
}
