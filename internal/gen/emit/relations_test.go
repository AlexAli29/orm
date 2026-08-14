package emit_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/model"
)

// The relation emitter's tests. They build the mapping reconciliation would
// have produced, because what is being checked is the code written for a proved
// relation rather than the proving.

// related returns a User/Post mapping with one relation each way: User.Posts is
// to-many over posts.author_id, and Post.Author is to-one back to users.id.
//
// nullableFK makes posts.author_id nullable, which is the difference between a
// post that must have an author and one that need not.
func related(t *testing.T, nullableFK bool) map[string]string {
	t.Helper()

	users := mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "Email", goType: "string", column: "email", pgType: tText},
	)
	posts := mapping("Post", "posts",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
		col{field: "Title", goType: "string", column: "title", pgType: tText},
		col{field: "AuthorID", goType: "int64", column: "author_id", pgType: tInt8, nullable: nullableFK},
	)
	um, pm := users.Entities[0], posts.Entities[0]

	fk := &model.PGForeignKey{
		Name:     "posts_author_id_fkey",
		Table:    pm.Table,
		Cols:     []*model.PGColumn{pm.Table.Cols[2]},
		RefTable: um.Table,
		RefCols:  []*model.PGColumn{um.Table.Cols[0]},
	}
	pm.Table.FKs = append(pm.Table.FKs, fk)

	um.Entity.Fields = append(um.Entity.Fields, model.GoField{
		Name: "Posts", Type: model.GoType{Src: "orm.Many[Post]", Value: "orm.Many[Post]"},
	})
	um.Rels = []model.RelMapping{{
		Field:       &um.Entity.Fields[len(um.Entity.Fields)-1],
		Idx:         len(um.Entity.Fields) - 1,
		Cardinality: model.CardMany,
		FKSide:      model.FKRemote,
		FK:          fk,
		KeyCols:     []model.RelKeyCol{{Column: um.Table.Cols[0], FieldIdx: 0}},
		TargetCols:  []model.RelKeyCol{{Column: pm.Table.Cols[2], FieldIdx: 2}},
		Target:      pm,
	}}

	pm.Entity.Fields = append(pm.Entity.Fields, model.GoField{
		Name: "Author", Type: model.GoType{Src: "orm.One[User]", Value: "orm.One[User]"},
	})
	pm.Rels = []model.RelMapping{{
		Field:       &pm.Entity.Fields[len(pm.Entity.Fields)-1],
		Idx:         len(pm.Entity.Fields) - 1,
		Cardinality: model.CardOne,
		FKSide:      model.FKLocal,
		FK:          fk,
		KeyCols:     []model.RelKeyCol{{Column: pm.Table.Cols[2], FieldIdx: 2}},
		TargetCols:  []model.RelKeyCol{{Column: um.Table.Cols[0], FieldIdx: 0}},
		Target:      um,
	}}

	m := &model.Mapping{Entities: []*model.EntityMapping{um, pm}}
	return generate(t, m)
}

func TestGenerate_relationDescriptors(t *testing.T) {
	files := related(t, false)
	got, ok := files["orm_rel.gen.go"]
	if !ok {
		t.Fatalf("no relations file; generated %v", keys(files))
	}

	for _, want := range []string{
		// The key travels with the PostgreSQL type name, because the loader
		// casts the array of parent keys with it.
		"var userPostsKeys = []orm.RelKey{\n\t{Parent: \"id\", Target: \"author_id\", Type: \"int8\"},\n}",
		"var postAuthorKeys = []orm.RelKey{\n\t{Parent: \"author_id\", Target: \"id\", Type: \"int8\"},\n}",
		// Column order is the order the scanner reads them in, so it has to be
		// the mapping's rather than anything sorted or ranged over a map.
		"var userPostsColumns = []string{\n\t\"id\",\n\t\"title\",\n\t\"author_id\",\n}",
		"var postAuthorColumns = []string{\n\t\"id\",\n\t\"email\",\n}",
	} {
		if !hasCode(got, want) {
			t.Errorf("orm_rel.gen.go does not contain:\n%s\n\ngot:\n%s", want, got)
		}
	}

	// The descriptors themselves live beside the column descriptors, on the
	// table type, so that Users.Posts reads the way Users.Email does.
	tables := files["orm_tables.gen.go"]
	for _, want := range []string{
		`Posts orm.Rel[User, Post]`,
		`Author orm.Rel[Post, User]`,
		`Posts: orm.NewManyRel(orm.ManyRelSpec[User, Post]{`,
		`Author: orm.NewOneRel(orm.OneRelSpec[Post, User]{`,
		// The parent occurrence is the src the descriptors are being built for,
		// so an aliased table yields relations correlated against the alias.
		// The target is the target's own occurrence, because that is the one a
		// caller's relation predicates name.
		"Parent: src,",
		"Target: postsSource,",
		"Target: usersSource,",
	} {
		if !hasCode(tables, want) {
			t.Errorf("orm_tables.gen.go does not contain %q:\n%s", want, tables)
		}
	}
}

// The mirror is what makes an absent to-one relation scannable: a LEFT JOIN
// that matched nothing returns NULL for every target column, including the ones
// the entity declares as plain values.
func TestGenerate_relationNullMirror(t *testing.T) {
	got := related(t, false)["orm_rel.gen.go"]

	for _, want := range []string{
		`type userNulls struct {`,
		"ID    *int64",
		"Email *string",
		`func userNullDest(n *userNulls) []any {`,
		`func userFromNulls(n *userNulls) (User, bool) {`,
		// Presence is the primary key, which PostgreSQL guarantees is not NULL
		// for a real row. Any other column could be NULL in a row that exists.
		`if n.ID == nil {`,
		`return User{}, false`,
	} {
		if !hasCode(got, want) {
			t.Errorf("orm_rel.gen.go does not contain:\n%s\n\ngot:\n%s", want, got)
		}
	}

	// Only the target of a to-one relation needs a mirror. Posts are batched, so
	// they are scanned from their own statement where no column is spuriously
	// NULL, and a mirror for them would be dead code.
	if strings.Contains(got, "type postNulls struct {") {
		t.Errorf("a batched relation's target got a null mirror it cannot use:\n%s", got)
	}
}

// A nullable foreign key changes nothing about how the relation loads: the key
// is still one column matched against one column. What it changes is the Go
// type the key arrives in, and the loader has to unwrap it before sending it.
func TestGenerate_relationOverANullableKey(t *testing.T) {
	got := related(t, true)["orm_rel.gen.go"]
	if !hasCode(got, "var postAuthorKeys = []orm.RelKey{\n\t{Parent: \"author_id\", Target: \"id\", Type: \"int8\"},\n}") {
		t.Errorf("the key is not described the same way over a nullable column:\n%s", got)
	}
	// The mirror already wrapped every field in a pointer; a nullable field
	// must not come out doubly wrapped.
	if strings.Contains(got, "**int64") {
		t.Errorf("a nullable field was wrapped twice:\n%s", got)
	}
}

// The batched loader is the one piece that must not be per-parent: it takes
// every parent's key at once and hands the whole set to the runtime.
func TestGenerate_batchedLoader(t *testing.T) {
	got := related(t, false)["orm_rel.gen.go"]
	for _, want := range []string{
		`func userPostsLoadKeys(parents []*User) ([]any, error) {`,
		`k0 := make([]int64, len(parents))`,
		`k0[i] = p.ID`,
		// The rows are scanned through the target's own scanner and attached by
		// a function of its own, both of which the runtime calls. Neither is a
		// closure inside a generated loader that only PostgreSQL could reach.
		`func postRelDest(t *Post) []any {`,
		`func userPostsAttach(e *User, rows []Post) { e.Posts = orm.NewManyFrom(rows) }`,
		// The refs reader is what hands one level's rows to the next.
		`func userPostsRefs(e *User, out []*Post) []*Post {`,
		`out = append(out, &rows[i])`,
	} {
		if !hasCode(got, want) {
			t.Errorf("orm_rel.gen.go does not contain:\n%s\n\ngot:\n%s", want, got)
		}
	}
}

// A package that declares no relation gets no relations file. An empty one
// would be a permanent diff in every project that does not use them.
func TestGenerate_noRelationsNoFile(t *testing.T) {
	files := generate(t, mapping("User", "users",
		col{field: "ID", goType: "int64", column: "id", pgType: tInt8},
	))
	if _, ok := files["orm_rel.gen.go"]; ok {
		t.Errorf("generated a relations file for a package with no relations: %v", keys(files))
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
