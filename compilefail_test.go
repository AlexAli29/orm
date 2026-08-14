package orm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The compile-fail suite.
//
// These are product guarantees, not conveniences. The point of a typed query
// builder is that the mistakes it is built to prevent are rejected by the Go
// compiler rather than reported at run time by PostgreSQL, and the only way to
// assert that a program does not compile is to try to compile it.
//
// Each case is a whole program written into a temporary module and built. The
// module has a replace directive pointing back at this checkout, so what is
// being compiled against is this working tree, not a published version.

// preamble declares two entities and enough descriptors to make every mistake
// below expressible. It stands in for generated code: the descriptor types are
// the ones the generator picks for these column shapes, so a case that fails to
// compile here would fail to compile against real output.
const preamble = `package main

import (
	"time"

	"github.com/AlexAli29/orm"
)

type User struct {
	ID        int64
	Email     string
	Age       int32
	Nickname  *string
	ManagerID *int64
	CreatedAt time.Time
	Posts     orm.Many[Post]
}

type Post struct {
	ID        int64
	Title     string
	Published bool
	CreatedAt time.Time
	Author    orm.One[User]
	Comments  orm.Many[Comment]
}

type Comment struct {
	ID   int64
	Body string
}

var usersSource = orm.NewSource("public", "users")
var postsSource = orm.NewSource("public", "posts")
var commentsSource = orm.NewSource("public", "comments")

type userTable struct {
	src       *orm.Source
	ID        orm.OrdCol[User, int64]
	Email     orm.TextCol[User]
	Age       orm.OrdCol[User, int32]
	Nickname  orm.NullTextCol[User]
	ManagerID orm.NullOrdCol[User, int64]
	CreatedAt orm.OrdCol[User, time.Time]
}

func newUserTable(src *orm.Source) userTable {
	return userTable{
		src:       src,
		ID:        orm.NewOrdCol[User, int64](src, "id"),
		Email:     orm.NewTextCol[User](src, "email"),
		Age:       orm.NewOrdCol[User, int32](src, "age"),
		Nickname:  orm.NewNullTextCol[User](src, "nickname"),
		ManagerID: orm.NewNullOrdCol[User, int64](src, "manager_id"),
		CreatedAt: orm.NewOrdCol[User, time.Time](src, "created_at"),
	}
}

func (t userTable) As(alias string) userTable   { return newUserTable(t.src.As(alias)) }
func (t userTable) Source() *orm.Source         { return t.src }

var Users = newUserTable(usersSource)

// M12.2: range descriptors, in the shape the generator emits them. The element
// type is a second parameter, which is what makes Range[int32] and
// Range[int64] two types rather than one.
type Booking struct {
	ID     int64
	Quota  orm.Range[int32]
	Span   orm.Range[int64]
	Period orm.Range[time.Time]
	Lease  orm.Interval
	Slots  orm.Multirange[int32]
}

var bookingsSource = orm.NewSource("public", "bookings")

type bookingTable struct {
	src    *orm.Source
	ID     orm.OrdCol[Booking, int64]
	Quota  orm.RangeCol[Booking, int32]
	Span   orm.RangeCol[Booking, int64]
	Period orm.RangeCol[Booking, time.Time]
	Lease  orm.OrdCol[Booking, orm.Interval]
	Slots  orm.MultirangeCol[Booking, int32]
}

func newBookingTable(src *orm.Source) bookingTable {
	return bookingTable{
		src:    src,
		ID:     orm.NewOrdCol[Booking, int64](src, "id"),
		Quota:  orm.NewRangeCol[Booking, int32](src, "quota", "int4range", "int4"),
		Span:   orm.NewRangeCol[Booking, int64](src, "span", "int8range", "int8"),
		Period: orm.NewRangeCol[Booking, time.Time](src, "period", "tstzrange", "timestamptz"),
		Lease:  orm.NewOrdCol[Booking, orm.Interval](src, "lease"),
		Slots:  orm.NewMultirangeCol[Booking, int32](src, "slots", "int4multirange", "int4range", "int4"),
	}
}

var Bookings = newBookingTable(bookingsSource)

// The relation descriptors, in the shape the generator emits them. Their bodies
// are never run: what is being asserted is which queries will accept them.
var UsersPosts = orm.NewManyRel(orm.ManyRelSpec[User, Post]{
	Name:        "Posts",
	Parent:      usersSource,
	Target:      postsSource,
	Keys:        []orm.RelKey{{Parent: "id", Target: "author_id", Type: "int8"}},
	Columns:     []string{"id", "title"},
	ExtractKeys: func([]*User) ([]any, error) { return nil, nil },
	Dest:        func(p *Post) []any { return []any{&p.ID} },
	Attach:      func(*User, []Post) {},
	Refs:        func(_ *User, out []*Post) []*Post { return out },
})

var PostsAuthor = orm.NewOneRel(orm.OneRelSpec[Post, User]{
	Name:        "Author",
	Parent:      postsSource,
	Target:      usersSource,
	Keys:        []orm.RelKey{{Parent: "author_id", Target: "id", Type: "int8"}},
	Columns:     []string{"id", "email"},
	Bind:        func() ([]any, func(*Post)) { return nil, func(*Post) {} },
	ExtractKeys: func([]*Post) ([]any, error) { return nil, nil },
	Dest:        func(u *User) []any { return []any{&u.ID} },
	Attach:      func(*Post, *User) {},
	Refs:        func(_ *Post, out []*User) []*User { return out },
})

var PostsComments = orm.NewManyRel(orm.ManyRelSpec[Post, Comment]{
	Name:        "Comments",
	Parent:      postsSource,
	Target:      commentsSource,
	Keys:        []orm.RelKey{{Parent: "id", Target: "post_id", Type: "int8"}},
	Columns:     []string{"id", "body"},
	ExtractKeys: func([]*Post) ([]any, error) { return nil, nil },
	Dest:        func(c *Comment) []any { return []any{&c.ID} },
	Attach:      func(*Post, []Comment) {},
	Refs:        func(_ *Post, out []*Comment) []*Comment { return out },
})

var Posts = struct {
	ID        orm.OrdCol[Post, int64]
	Title     orm.TextCol[Post]
	Published orm.Col[Post, bool]
	CreatedAt orm.OrdCol[Post, time.Time]
}{
	ID:        orm.NewOrdCol[Post, int64](postsSource, "id"),
	Title:     orm.NewTextCol[Post](postsSource, "title"),
	Published: orm.NewCol[Post, bool](postsSource, "published"),
	CreatedAt: orm.NewOrdCol[Post, time.Time](postsSource, "created_at"),
}

var userMeta = orm.EntityMeta[User]{
	Table:   orm.TableID{Schema: "public", Name: "users"},
	Source:  usersSource,
	Columns: []orm.ColumnMeta{{Name: "id", Field: "ID"}},
	Dest:    func(e *User, idx int) any { return &e.ID },
}

var postMeta = orm.EntityMeta[Post]{
	Table:   orm.TableID{Schema: "public", Name: "posts"},
	Source:  postsSource,
	Columns: []orm.ColumnMeta{{Name: "id", Field: "ID"}},
	Dest:    func(e *Post, idx int) any { return &e.ID },
}

type DB struct {
	Users *orm.Repo[User]
	Posts *orm.Repo[Post]
}

var db = DB{
	Users: orm.NewRepo[User](nil, &userMeta),
	Posts: orm.NewRepo[Post](nil, &postMeta),
}

func main() { _ = db }
`

func TestCompileFails(t *testing.T) {
	dir := compileFailModule(t)

	tests := []struct {
		name string
		body string
		// want is a fragment of the compiler's complaint, asserted so that the
		// case fails for the reason it is about rather than a typo.
		want string
	}{
		{
			name: "an int4range asked to contain an int64",
			body: `func bad() { _ = Bookings.Quota.Contains(int64(3)) }`,
			want: "cannot use",
		},
		{
			name: "an int4range asked to contain a time",
			body: `func bad() { _ = Bookings.Quota.Contains(time.Now()) }`,
			want: "cannot use",
		},
		{
			name: "an int4range compared to an int8range",
			body: `func bad() { _ = Bookings.Quota.Overlaps(orm.ClosedOpen[int64](1, 2)) }`,
			want: "cannot use",
		},
		{
			name: "a timestamp range compared to an integer range",
			body: `func bad() { _ = Bookings.Period.Overlaps(orm.ClosedOpen[int32](1, 2)) }`,
			want: "cannot use",
		},
		{
			name: "two range columns of different element types compared",
			body: `func bad() { _ = Bookings.Quota.OverlapsCol(Bookings.Span) }`,
			want: "cannot use",
		},
		{
			name: "a range expression over the wrong element type",
			body: `func bad() {
	_ = orm.RangeContains(orm.Of(Bookings.Quota), orm.Of(Users.CreatedAt))
}`,
			want: "does not match inferred type",
		},
		{
			name: "a multirange asked to contain the wrong element",
			body: `func bad() { _ = Bookings.Slots.Contains(int64(3)) }`,
			want: "cannot use",
		},
		{
			name: "a NOT NULL range asked whether it is null",
			body: `func bad() { _ = Bookings.Quota.IsNull() }`,
			want: "IsNull undefined",
		},
		{
			name: "lower() of a range bound to a non-nullable destination",
			body: `func bad() {
	_ = orm.Project1(Bookings.Quota.Lower(), func(v int32) int32 { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "an interval assigned from a duration",
			body: `func bad() { var iv orm.Interval = time.Hour; _ = iv }`,
			want: "cannot use",
		},
		{
			name: "a range value assigned across element types",
			body: `func bad() { var r orm.Range[int64] = orm.ClosedOpen[int32](1, 2); _ = r }`,
			want: "cannot use",
		},
		{
			name: "a column compared to the wrong value type",
			body: `func bad() { _ = Users.Age.Eq("18") }`,
			want: "cannot use",
		},
		{
			name: "a NOT NULL column asked whether it is null",
			body: `func bad() { _ = Users.Email.IsNull() }`,
			want: "IsNull undefined",
		},
		{
			name: "a NOT NULL column asked whether it is not null",
			body: `func bad() { _ = Users.Email.IsNotNull() }`,
			want: "IsNotNull undefined",
		},
		{
			name: "another entity's predicate in Where",
			body: `func bad() { _ = db.Users.Query().Where(Posts.Published.Eq(true)) }`,
			want: "cannot use",
		},
		{
			name: "two entities mixed in And",
			body: `func bad() { _ = orm.And(Users.Age.Eq(18), Posts.Published.Eq(true)) }`,
			want: "",
		},
		{
			name: "another entity's column in OrderBy",
			body: `func bad() { _ = db.Users.Query().OrderBy(Posts.CreatedAt.Desc()) }`,
			want: "cannot use",
		},
		{
			name: "a base column asked for a magnitude comparison",
			body: `func bad() { _ = Posts.Published.Gt(true) }`,
			want: "Gt undefined",
		},
		{
			name: "a non-text column asked to match a pattern",
			body: `func bad() { _ = Users.Age.ILike("%18%") }`,
			want: "ILike undefined",
		},
		{
			name: "an ordered comparison against the wrong type",
			body: `func bad() { _ = Users.CreatedAt.Gte("yesterday") }`,
			want: "cannot use",
		},
		{
			// EqCol compares two columns, so both sides have to hold the same
			// type. id is int64 and email is text.
			name: "two columns of different types compared",
			body: `func bad() { _ = Users.ID.EqCol(Users.Email) }`,
			want: "does not implement",
		},
		{
			name: "a column compared to another entity's column",
			body: `func bad() { _ = Users.ID.EqCol(Posts.ID) }`,
			want: "does not implement",
		},
		{
			name: "an alias compared to another entity's column",
			body: `func bad() { m := Users.As("m"); _ = m.ID.EqCol(Posts.ID) }`,
			want: "does not implement",
		},
		{
			name: "another entity's raw predicate",
			body: `func bad() { _ = db.Users.Query().Where(orm.Expr[Post]("x = $1", 1)) }`,
			want: "cannot use",
		},
		// M4: the write API carries the same guarantees.
		{
			name: "an assignment of the wrong value type",
			body: `func bad() { _ = Users.Age.Set("18") }`,
			want: "cannot use",
		},
		{
			name: "another entity's assignment",
			body: `func bad() { _ = db.Users.Update().Set(Posts.Title.Set("x")) }`,
			want: "cannot use",
		},
		{
			name: "SetNull on a NOT NULL column",
			body: `func bad() { _ = Users.Email.SetNull() }`,
			want: "SetNull undefined",
		},
		{
			name: "two entities mixed in Default",
			body: `func bad() { _ = orm.Default(Users.CreatedAt, Posts.CreatedAt) }`,
			want: "",
		},
		{
			name: "two entities mixed in a conflict target",
			body: `func bad() { _ = orm.OnConflict(Users.Email, Posts.ID).DoNothing() }`,
			want: "",
		},
		{
			name: "another entity's column in DoUpdate",
			body: `func bad() { _ = orm.OnConflict(Users.Email).DoUpdate(Posts.Title) }`,
			want: "cannot use",
		},
		{
			name: "another entity's option passed to an insert",
			body: `func bad() { _, _ = db.Users.Insert(nil, User{}, orm.Default(Posts.CreatedAt)) }`,
			want: "cannot use",
		},
		{
			name: "another entity's predicate on a delete",
			body: `func bad() { _ = db.Users.Delete().Where(Posts.Published.Eq(true)) }`,
			want: "cannot use",
		},
		{
			name: "another entity's predicate on an update",
			body: `func bad() { _ = db.Users.Update().Set(Users.Age.Set(1)).Where(Posts.Published.Eq(true)) }`,
			want: "cannot use",
		},
		{
			name: "another entity's relation in With",
			body: `func bad() { _ = db.Users.Query().With(PostsAuthor) }`,
			want: "cannot use",
		},
		{
			name: "the parent's own relation requested from the target's query",
			body: `func bad() { _ = db.Posts.Query().With(UsersPosts) }`,
			want: "cannot use",
		},
		{
			name: "a relation assigned to the wrong entity's loader",
			body: `func bad() { var l orm.Loader[Post] = UsersPosts; _ = l }`,
			want: "cannot use",
		},
		{
			name: "one entity's relations mixed with another's in a single With",
			body: `func bad() { _ = db.Users.Query().With(UsersPosts, PostsAuthor) }`,
			want: "cannot use",
		},
		{
			name: "another entity's predicate configuring a relation",
			body: `func bad() { _ = UsersPosts.Where(Users.Age.Eq(int32(1))) }`,
			want: "cannot use",
		},
		{
			name: "another entity's ordering configuring a relation",
			body: `func bad() { _ = UsersPosts.OrderBy(Users.CreatedAt.Desc()) }`,
			want: "cannot use",
		},
		{
			name: "another entity's predicate inside Any",
			body: `func bad() { _ = UsersPosts.Any(Users.Age.Eq(int32(1))) }`,
			want: "cannot use",
		},
		{
			name: "another entity's predicate inside None",
			body: `func bad() { _ = UsersPosts.None(Users.Age.Eq(int32(1))) }`,
			want: "cannot use",
		},
		{
			// Any answers a question about the parent, so it yields a predicate
			// over the parent — which the target's own query cannot take.
			name: "a semi-join over the wrong root query",
			body: `func bad() { _ = db.Posts.Query().Where(UsersPosts.Any()) }`,
			want: "cannot use",
		},
		{
			name: "a semi-join predicate combined with the target's own",
			body: `func bad() { _ = orm.And(UsersPosts.Any(), Posts.Published.Eq(true)) }`,
			want: "does not match inferred type",
		},
		{
			// The relation targets User, so a Post predicate cannot configure
			// it even though both entities exist in this program.
			name: "the wrong target's predicate on a to-one relation",
			body: `func bad() { _ = PostsAuthor.Where(Posts.Published.Eq(true)) }`,
			want: "cannot use",
		},
		{
			name: "a configured relation handed to the wrong query",
			body: `func bad() { _ = db.Posts.Query().With(UsersPosts.Limit(5)) }`,
			want: "cannot use",
		},
		{
			// A nested With takes relations of the target, and User is not the
			// target of Users.Posts.
			name: "the parent's own relation nested under it",
			body: `func bad() { _ = UsersPosts.With(UsersPosts) }`,
			want: "cannot use",
		},
		{
			name: "a relation of a third entity nested under one",
			body: `func bad() { _ = PostsComments.With(UsersPosts) }`,
			want: "cannot use",
		},
		{
			name: "a nested relation of the wrong depth",
			body: `func bad() { _ = UsersPosts.With(PostsComments.With(PostsComments)) }`,
			want: "cannot use",
		},
		{
			name: "another entity's predicate configuring a nested relation",
			body: `func bad() { _ = UsersPosts.With(PostsComments.Where(Users.Age.Eq(int32(1)))) }`,
			want: "cannot use",
		},
		{
			name: "a nested relation tree handed to the wrong query",
			body: `func bad() { _ = db.Posts.Query().With(UsersPosts.With(PostsComments)) }`,
			want: "cannot use",
		},
		{
			name: "a range over mismatched types",
			body: `func bad() { _ = Users.Age.Between(int32(1), "2") }`,
			want: "cannot use",
		},

		// M10.1: a projection binds expressions to a function of exactly their
		// result types, so every mismatch between what is selected and what
		// reads it back is a type error rather than a scan failure.
		{
			name: "an int column bound to a string result",
			body: `func bad() {
	_ = orm.Project1(Users.ID, func(s string) string { return s })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a nullable column bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(Users.Nickname, func(s string) string { return s })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a non-nullable column bound to a pointer result",
			body: `func bad() {
	_ = orm.Project1(Users.Email, func(s *string) string { return *s })
}`,
			want: "does not match inferred type",
		},
		{
			name: "fewer binder parameters than expressions",
			body: `func bad() {
	_ = orm.Project2(Users.ID, Users.Email, func(id int64) int64 { return id })
}`,
			want: "does not match inferred type",
		},
		{
			name: "more binder parameters than expressions",
			body: `func bad() {
	_ = orm.Project1(Users.ID, func(id int64, extra string) int64 { return id })
}`,
			want: "does not match inferred type",
		},
		{
			name: "another entity's column in a projection",
			body: `func bad() {
	_ = orm.Project2(Users.ID, Posts.Title, func(id int64, t string) int64 { return id })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a projection handed to the wrong repository",
			body: `func bad() {
	p := orm.Project1(Posts.ID, func(id int64) int64 { return id })
	_ = orm.Select(db.Users, p)
}`,
			want: "does not match inferred type",
		},
		{
			name: "another entity's predicate on a projection query",
			body: `func bad() {
	p := orm.Project1(Users.ID, func(id int64) int64 { return id })
	_ = orm.Select(db.Users, p).Where(Posts.Published.Eq(true))
}`,
			want: "cannot use",
		},
		{
			name: "another entity's ordering on a projection query",
			body: `func bad() {
	p := orm.Project1(Users.ID, func(id int64) int64 { return id })
	_ = orm.Select(db.Users, p).OrderBy(Posts.CreatedAt.Desc())
}`,
			want: "cannot use",
		},
		{
			name: "a raw value whose stated type is not the destination",
			body: `func bad() {
	_ = orm.Project1(orm.RawValue[User, int64]("1"), func(s string) string { return s })
}`,
			want: "does not match inferred type",
		},
		// M10.2: an aggregate's result type is PostgreSQL's, so binding it to
		// the type Go would have guessed is a compile error.
		{
			name: "a nullable aggregate bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(orm.SumInt32(Users.Age), func(n int64) int64 { return n })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a sum of integer bound to the input type",
			body: `func bad() {
	_ = orm.Project1(orm.SumInt32(Users.Age), func(n *int32) int32 { return *n })
}`,
			want: "does not match inferred type",
		},
		{
			name: "an aggregate over another entity's column",
			body: `func bad() {
	p := orm.Project1(orm.Min(Posts.CreatedAt), func(v *time.Time) time.Time { return *v })
	_ = orm.Select(db.Users, p)
}`,
			want: "does not match inferred type",
		},
		{
			name: "a sum over a column of the wrong width",
			body: `func bad() { _ = orm.SumInt16(Users.Age) }`,
			want: "does not match inferred type",
		},
		{
			name: "a count compared to the wrong type",
			body: `func bad() { _ = orm.Count[User]().Gt("many") }`,
			want: "cannot use",
		},
		{
			name: "an aggregate filtered by another entity's predicate",
			body: `func bad() { _ = orm.Count[User]().Filter(Posts.Published.Eq(true)) }`,
			want: "cannot use",
		},
		{
			name: "another entity's grouping on a projection query",
			body: `func bad() {
	p := orm.Project1(Users.ID, func(id int64) int64 { return id })
	_ = orm.Select(db.Users, p).GroupBy(Posts.Published)
}`,
			want: "cannot use",
		},
		{
			name: "another entity's aggregate in Having",
			body: `func bad() {
	p := orm.Project1(Users.ID, func(id int64) int64 { return id })
	_ = orm.Select(db.Users, p).Having(orm.Count[Post]().Gt(1))
}`,
			want: "cannot use",
		},
		// M10.3: an assignment's right-hand side is typed by the column it
		// writes, in both its value type and its nullability.
		{
			name: "a text expression assigned to an integer column",
			body: `func bad() { _ = Users.Age.SetExpr(Users.Email.Value()) }`,
			want: "cannot use",
		},
		{
			name: "another entity's expression in an assignment",
			body: `func bad() { _ = Users.Age.SetExpr(orm.RawValue[Post, int32]("1")) }`,
			want: "cannot use",
		},
		{
			name: "a nullable expression assigned to a NOT NULL column",
			body: `func bad() { _ = Users.Age.SetExpr(orm.Nullable(Users.Age)) }`,
			want: "cannot use",
		},
		{
			name: "a non-nullable expression assigned to a nullable column",
			body: `func bad() { _ = Users.Nickname.SetExpr(Users.Email.Value()) }`,
			want: "cannot use",
		},
		{
			name: "arithmetic between mismatched types",
			body: `func bad() { _ = Users.Age.Add(int64(1)) }`,
			want: "cannot use",
		},
		{
			name: "arithmetic against another entity's column",
			body: `func bad() { _ = Users.Age.AddCol(Posts.ID) }`,
			want: "does not implement",
		},
		{
			name: "an EXCLUDED reference to another entity's column",
			body: `func bad() { _ = Users.Age.SetExpr(orm.Excluded(Posts.ID)) }`,
			want: "cannot use",
		},
		{
			name: "a returning shape from the wrong entity",
			body: `func bad() {
	p := orm.Project1(Posts.ID, func(id int64) int64 { return id })
	_ = orm.UpdateReturning(db.Users.Update(), p)
}`,
			want: "does not match inferred type",
		},
		{
			name: "another entity's assignment in a conflict clause",
			body: `func bad() {
	_ = orm.OnConflict(Users.Email).DoUpdateSet(Posts.Title.Set("x"))
}`,
			want: "cannot use",
		},
		// AUDIT: a value expression over a nullable column is nullable. These
		// were all accepted before, which let a NULL reach a destination that
		// cannot hold one.
		{
			name: "a nullable column's value bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(Users.Nickname.Value(), func(s string) string { return s })
}`,
			want: "does not match inferred type",
		},
		{
			name: "arithmetic over a nullable column bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(Users.ManagerID.Add(1), func(n int64) int64 { return n })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a nullable column assigned to a NOT NULL column",
			body: `func bad() { _ = Users.Email.SetExpr(Users.Nickname.Value()) }`,
			want: "cannot use",
		},
		{
			name: "arithmetic over a nullable column assigned to a NOT NULL column",
			body: `func bad() { _ = Users.Age.SetExpr(Users.ManagerID.Add(1)) }`,
			want: "cannot use",
		},
		{
			name: "arithmetic against a nullable operand",
			body: `func bad() { _ = Users.ID.AddCol(Users.ManagerID) }`,
			want: "does not implement",
		},
		{
			name: "EXCLUDED of a nullable column claimed non-null",
			body: `func bad() { _ = Users.Email.SetExpr(orm.Excluded(Users.Nickname)) }`,
			want: "cannot use",
		},
		{
			name: "a result alias does not change the result type",
			body: `func bad() {
	_ = orm.Project1(Users.Nickname.As("nick"), func(s string) string { return s })
}`,
			want: "does not match inferred type",
		},

		// M11.1: a derived table's columns are typed by the expressions the
		// inner query selected, so reading one back at another type is a
		// mistake the compiler makes rather than a scan failure.
		{
			name: "a derived column bound to the wrong result type",
			body: `func bad() {
	n := orm.Named("n", orm.Of(Users.ID))
	s := orm.Sub("s", orm.Rows(n).From(usersSource))
	_ = orm.Project1(orm.Ref(s, n), func(v string) string { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a derived column read through an outer join bound to a non-nullable result",
			body: `func bad() {
	n := orm.Named("n", orm.Of(Users.ID))
	s := orm.Sub("s", orm.Rows(n).From(usersSource))
	_ = orm.Project1(orm.OptRef(s, n), func(v int64) int64 { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a nullable derived column bound to a non-nullable result",
			body: `func bad() {
	n := orm.Named("n", orm.Of(Users.Nickname))
	s := orm.Sub("s", orm.Rows(n).From(usersSource))
	_ = orm.Project1(orm.Ref(s, n), func(v string) string { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a scalar subquery bound to a non-nullable result",
			body: `func bad() {
	n := orm.Named("n", orm.Of(Users.ID))
	_ = orm.Project1(orm.Scalar[User, int64](orm.Rows(n).From(usersSource)),
		func(v int64) int64 { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "two columns of different value types compared across sources",
			body: `func bad() { _ = orm.Eq(Users.ID, Users.Email) }`,
			want: "does not match",
		},
		// M11.5: a window function's result type is PostgreSQL's, and lag and
		// lead are nullable because the row they read may not exist.
		{
			name: "row_number bound to a non-bigint result",
			body: `func bad() {
	_ = orm.Project1(orm.RowNumber().Over(nil), func(n int32) int32 { return n })
}`,
			want: "does not match inferred type",
		},
		{
			name: "lag bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(orm.Lag(Users.ID).Over(nil), func(v int64) int64 { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "lead bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(orm.Lead(Users.Email).Over(nil), func(v string) string { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a windowed sum bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(orm.OfNull(orm.SumInt32(Users.Age).Over(nil)), func(v int64) int64 { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a window function selected before it has a window",
			body: `func bad() { _ = orm.Project1(orm.RowNumber(), func(n int64) int64 { return n }) }`,
			want: "does not match",
		},

		// M11.4: an advanced expression states its result type and its
		// nullability, and both are checked where the result is bound.
		{
			name: "a CASE branch of another type",
			body: `func bad() {
	_ = orm.Case(orm.Cond(Users.Age.Eq(int32(1))), orm.Val("a")).
		When(orm.Cond(Users.Age.Eq(int32(2))), orm.Val(int64(3)))
}`,
			want: "cannot use",
		},
		{
			name: "a CASE with no ELSE bound to a non-nullable result",
			body: `func bad() {
	c := orm.Case(orm.Cond(Users.Age.Eq(int32(1))), orm.Val("a")).End()
	_ = orm.Project1(c, func(s string) string { return s })
}`,
			want: "does not match inferred type",
		},
		{
			name: "NULLIF bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(orm.NullIf(Users.Email, orm.Val("x")), func(s string) string { return s })
}`,
			want: "does not match inferred type",
		},
		{
			name: "COALESCE over a fallback of another type",
			body: `func bad() { _ = orm.Coalesce(orm.Val(int64(0)), orm.Of(Users.Nickname)) }`,
			want: "does not match",
		},
		{
			name: "a cast bound to a type the target does not produce",
			body: `func bad() {
	_ = orm.Project1(orm.Cast(Users.ID, orm.Text), func(v int64) int64 { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a nullable cast bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(orm.CastNull(Users.Nickname, orm.Text), func(s string) string { return s })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a string function over a non-text column",
			body: `func bad() { _ = orm.Lower(Users.ID) }`,
			want: "does not match",
		},
		{
			name: "an array operator over mismatched element types",
			body: `func bad() { _ = orm.AnyOf(Users.Email, orm.Val([]int64{1})) }`,
			want: "does not match",
		},
		{
			name: "a tuple comparison against values of the wrong types",
			body: `func bad() { _ = orm.Row2Eq(Users.ID, Users.Email, "one", "two") }`,
			want: "cannot use",
		},

		// M11.2: a value read through an outer join is nullable, and Opt is how
		// the type says so. Binding what Opt produced to a non-pointer is the
		// mistake the widening exists to prevent.
		{
			name: "an outer-joined column bound to a non-nullable result",
			body: `func bad() {
	_ = orm.Project1(orm.Opt(Users.ID), func(v int64) int64 { return v })
}`,
			want: "does not match inferred type",
		},
		{
			name: "a composed predicate handed to an entity query",
			body: `func bad() {
	_ = db.Users.Query().Where(orm.Eq(Users.ID, Users.ID))
}`,
			want: "cannot use",
		},
		{
			name: "an entity predicate handed to a composed query",
			body: `func bad() {
	shape := orm.Project1(orm.Of(Users.ID), func(v int64) int64 { return v })
	_ = orm.Compose(nil, shape).From(usersSource).Where(Users.Age.Eq(int32(1)))
}`,
			want: "cannot use",
		},
		{
			name: "an entity ordering handed to a composed query",
			body: `func bad() {
	shape := orm.Project1(orm.Of(Users.ID), func(v int64) int64 { return v })
	_ = orm.Compose(nil, shape).From(usersSource).OrderBy(Users.ID.Asc())
}`,
			want: "cannot use",
		},
		{
			name: "an entity expression selected into a composed query without being lifted",
			body: `func bad() {
	_ = orm.Compose(nil, orm.Project1(Users.ID, func(v int64) int64 { return v }))
}`,
			want: "does not match",
		},

		// UNION ALL. Two halves of the negative matrix are settled here rather
		// than at run time: branches that produce different Go types, and types
		// outside this package pretending to be branches. Everything the
		// compiler cannot see — column counts, per-column types, nullability —
		// is refused when the union is built, and is asserted in union_test.go.
		{
			name: "branches producing different Go result types",
			body: `func bad() {
	ids := orm.Compose(nil, orm.Project1(orm.Of(Users.ID), func(v int64) int64 { return v })).From(usersSource)
	names := orm.Compose(nil, orm.Project1(orm.Of(Users.Email), func(v string) string { return v })).From(usersSource)
	_ = orm.UnionAll(ids, names)
}`,
			want: "does not match",
		},
		{
			name: "entity branches of different entities",
			body: `func bad() { _ = orm.UnionAll(db.Users.Query(), db.Posts.Query()) }`,
			want: "does not match",
		},
		{
			name: "a projection branch unioned with an entity branch of another type",
			body: `func bad() {
	ids := orm.Compose(nil, orm.Project1(orm.Of(Users.ID), func(v int64) int64 { return v })).From(usersSource)
	_ = orm.UnionAll(ids, db.Users.Query())
}`,
			want: "does not match",
		},
		{
			// Branch is closed by an unexported method, so a caller cannot
			// hand a union something that arrives without a result shape.
			name: "a type outside the package claiming to be a branch",
			body: `type fake struct{}

func bad() {
	ids := orm.Compose(nil, orm.Project1(orm.Of(Users.ID), func(v int64) int64 { return v })).From(usersSource)
	_ = orm.UnionAll[int64](ids, fake{})
}`,
			want: "orm.Branch",
		},
		{
			// The internal compound node is not reachable, so validation cannot
			// be walked around by building one.
			name: "a set operation assembled out of AST nodes",
			body: `func bad() { _ = orm.UnionAll[int64](nil, nil).Compound() }`,
			want: "Compound",
		},
		{
			// A compound may be ordered by an output column name and by nothing
			// else. An ordering term over an expression is what PostgreSQL
			// refuses — "invalid UNION/INTERSECT/EXCEPT ORDER BY clause" — so
			// the term for a set operation is a different type from the term
			// for a query, and the two do not substitute.
			//
			// This case previously asserted that ordering was not offered at
			// all. It is now offered, and what it asserts is the restriction
			// that made offering it safe.
			name: "ordering a set operation by a table's column",
			body: `func bad() {
	ids := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"), func(v int64) int64 { return v })).From(usersSource)
	_ = orm.UnionAll(ids, ids).OrderBy(orm.Of(Users.ID).Asc())
}`,
			want: "orm.OutputOrder",
		},
		{
			name: "ordering a set operation by an entity's column",
			body: `func bad() {
	ids := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"), func(v int64) int64 { return v })).From(usersSource)
	_ = orm.UnionAll(ids, ids).OrderBy(Users.ID.Asc())
}`,
			want: "orm.OutputOrder",
		},
		{
			// And the other way: an output-column term orders a set operation
			// and not a query, because a query's ORDER BY takes an expression
			// and this names a column of a result.
			name: "ordering a query by an output column term",
			body: `func bad() {
	outID := orm.Named("id", orm.Of(Users.ID))
	_ = orm.Compose(nil, orm.Project1(orm.Of(Users.ID), func(v int64) int64 { return v })).
		From(usersSource).OrderBy(outID.Asc())
}`,
			want: "orm.Order",
		},

		// Compound queries as row sources. Sub and CTE take SourceTerm, which is
		// a different interface from Term rather than a widening of it: what can
		// be a source and what is a SELECT are separate claims, and the paths
		// that rewrite or compare select internals still demand the second.
		{
			// An entity query selects its descriptor's columns and names none of
			// them, so a derived table over it would have nothing for its columns
			// to be addressed by. This used to compile and fail when the
			// statement was built.
			name: "an entity query as a derived table",
			body: `func bad() { _ = orm.Sub("u", db.Users.Query()) }`,
			want: "orm.SourceTerm",
		},
		{
			name: "an entity query as a CTE body",
			body: `func bad() { _ = orm.CTE("u", db.Users.Query()) }`,
			want: "orm.SourceTerm",
		},
		{
			// SourceTerm is closed by an unexported method, so a caller cannot
			// hand Sub something that arrives without output names.
			name: "a type outside the package claiming to be a row source",
			body: `type fakeSource struct{}

func bad() { _ = orm.Sub("u", fakeSource{}) }`,
			want: "orm.SourceTerm",
		},
		{
			// A recursive CTE compares its anchor's select list against the
			// recursive term's, which only a Select can answer. A compound is
			// not one, so it cannot be an anchor.
			name: "a set operation as a recursive CTE's anchor",
			body: `func bad() {
	ids := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"), func(v int64) int64 { return v })).From(usersSource)
	_ = orm.RecursiveCTE("t", orm.UnionAll(ids, ids), func(self *orm.Source) orm.Term { return ids })
}`,
			want: "orm.Term",
		},
		{
			// EXISTS canonicalises the statement it is given, which is the same
			// concrete requirement.
			name: "a set operation inside EXISTS",
			body: `func bad() {
	ids := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"), func(v int64) int64 { return v })).From(usersSource)
	_ = orm.Exists[User](orm.UnionAll(ids, ids))
}`,
			want: "orm.Term",
		},

		// Value subqueries. A membership test and a scalar value nest the
		// statement they are given and read no part of it, so they take
		// ValueTerm — a third capability, separate from Term and from
		// SourceTerm, whose one requirement is that the query can say how many
		// columns its result has.
		//
		// A set operation as a scalar value used to be here, refused because
		// Scalar took Term. It is now valid and is asserted among the forms that
		// compile.
		{
			// ValueTerm is closed by an unexported method.
			name: "a type outside the package claiming to be a value subquery",
			body: `type fakeValue struct{}

func bad() { _ = orm.Scalar[User, int64](fakeValue{}) }`,
			want: "orm.ValueTerm",
		},
		{
			name: "a foreign type in a membership test",
			body: `type fakeValue struct{}

func bad() { _ = orm.InSub(Users.ID, fakeValue{}) }`,
			want: "orm.ValueTerm",
		},
		{
			// A write is a Subquery in the AST and a WriteTerm in the public
			// layer, and it is neither of the read capabilities. Reading the
			// rows a write touched is what WritingCTE is for.
			name: "a write used as a scalar value",
			body: `func bad() {
	shape := orm.Project1(Users.ID.As("id"), func(v int64) int64 { return v })
	_ = orm.Scalar[User, int64](orm.UpdateReturning(db.Users.Update(), shape))
}`,
			want: "orm.ValueTerm",
		},
		{
			// The two read capabilities are separate types, not one renamed. A
			// value known only to be a source cannot be nested as a value, and
			// the compiler says so rather than something failing later.
			name: "a source term used as a value subquery",
			body: `func bad() {
	var src orm.SourceTerm = orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"), func(v int64) int64 { return v })).From(usersSource)
	_ = orm.Scalar[User, int64](src)
}`,
			want: "orm.ValueTerm",
		},
		{
			name: "a value term used as a row source",
			body: `func bad() {
	var val orm.ValueTerm = db.Users.Query()
	_ = orm.Sub("u", val)
}`,
			want: "orm.SourceTerm",
		},
	}

	// The count is asserted because this suite's members pass by not compiling,
	// which is a property a deleted case has too. A case that is dropped — by a
	// bad merge, by a refactor that made it stop compiling for the wrong reason
	// and got removed, by an edit that folded two into one — takes its guarantee
	// with it and leaves the suite green and smaller.
	//
	// So the number is written down. Changing it is fine and takes one line;
	// changing it without noticing is what this stops.
	//
	// Its history, because a report of it was once irreconcilable. The suite was
	// 125 when a compound became a value subquery, and 127 when a compound
	// became orderable — three cases added and one deleted, not three added.
	// The deleted one asserted that ordering a set operation was not offered;
	// the case that replaced it asserts the restriction that made offering it
	// safe, which is a different guarantee in the same slot rather than the same
	// case reworded. A report that called it "changed meaning rather than
	// deleted" left a reader reconstructing 128.
	const cases = 127
	if len(tests) != cases {
		t.Errorf("the suite has %d cases and declares %d; a case that disappears leaves "+
			"this suite passing and smaller, which is why the number is written down",
			len(tests), cases)
	}
	seen := make(map[string]bool, len(tests))
	for _, tt := range tests {
		if seen[tt.name] {
			t.Errorf("two cases are named %q, so one of them is not being reported", tt.name)
		}
		seen[tt.name] = true
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := buildCase(t, dir, tt.body)
			if err == nil {
				t.Fatalf("this compiled, and it must not:\n%s", tt.body)
			}
			if tt.want != "" && !strings.Contains(out, tt.want) {
				t.Errorf("compiler said:\n%s\nwant it to mention %q", out, tt.want)
			}
		})
	}
}

// The harness itself, from both sides.
//
// Every case above passes by failing to compile, so a harness that reported
// failure unconditionally would make the whole suite green while proving
// nothing. These two are the sentinels: one program that must build and one that
// must not, checked against the same buildCase every case uses.
func TestCompileFails_theHarnessDistinguishesBothOutcomes(t *testing.T) {
	dir := compileFailModule(t)

	if out, err := buildCase(t, dir, `func fine() { _ = Users.Age.Eq(int32(18)) }`); err != nil {
		t.Errorf("the harness failed a program that compiles, so every case above "+
			"would pass whatever it contained:\n%s", out)
	}
	out, err := buildCase(t, dir, `func broken() { _ = Users.Age.Eq("not an int32") }`)
	if err == nil {
		t.Error("the harness passed a program that does not compile, so a case that " +
			"started compiling would be reported as still refused")
	}
	if err != nil && !strings.Contains(out, "cannot use") {
		t.Errorf("the harness reported a failure without the compiler's reason:\n%s", out)
	}
}

func TestCompileFails_theValidFormsStillCompile(t *testing.T) {
	// A suite that only proves things fail would pass just as well if nothing
	// compiled at all.
	dir := compileFailModule(t)
	body := `func good() {
	_ = Users.Age.Eq(18)
	_ = Users.Email.ILike("%a%")
	_ = Users.Nickname.IsNull()
	_ = Users.CreatedAt.Gte(time.Now())
	_ = orm.And(Users.Age.Gte(18), Users.Email.Like("%b%"))
	_ = orm.Or(Users.Age.Lt(18), orm.Not(Users.Age.Gt(65)))
	_ = db.Users.Query().Where(Users.Age.Eq(18)).OrderBy(Users.CreatedAt.Desc()).Limit(10)
	_ = Posts.Published.Eq(true)

	// M3: an alias of the same table keeps the same entity type, so a column
	// of it compares against a column of the original.
	manager := Users.As("manager")
	_ = manager.ID.EqCol(Users.ManagerID)
	_ = Users.ID.EqCol(Users.ManagerID)
	_ = orm.Expr[User]("score > $1", 100)

	// UNION ALL: two branches of one shape, read as one statement.
	ids := orm.Compose(nil, orm.Project1(orm.Of(Users.ID), func(v int64) int64 { return v })).From(usersSource)
	_ = orm.UnionAll(ids, ids).Limit(10)
	_ = orm.UnionAll(orm.UnionAll(ids, ids), ids)
	_ = orm.UnionAll(db.Users.Query(), db.Users.Query())

	// A set operation as a row source and as an ordinary CTE body. The
	// declarations name the first branch's columns, which is what PostgreSQL
	// calls a compound's output.
	named := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"), func(v int64) int64 { return v })).From(usersSource)
	outID := orm.Named("id", orm.Of(Users.ID))
	derived := orm.Sub("u", orm.UnionAll(named, named))
	_ = orm.Compose(nil, orm.Project1(orm.Ref(derived, outID), func(v int64) int64 { return v })).From(derived)
	item := orm.CTE("c", orm.UnionAll(named, named))
	_ = orm.Compose(nil, orm.Project1(orm.Ref(item, outID), func(v int64) int64 { return v })).With(item).From(item)
	// And the builders that were sources before still are.
	_ = orm.Sub("v", named)
	_ = orm.CTE("d", named)
	_ = orm.Sub("w", orm.Rows(outID).From(usersSource))

	// A set operation in a value position: one column, so both a membership
	// test and a scalar value take it.
	oneCol := orm.Compose(nil, orm.Project1(orm.Of(Users.ID), func(v int64) int64 { return v })).From(usersSource)
	_ = db.Users.Query().Where(orm.InSub(Users.ID, orm.UnionAll(oneCol, oneCol)))
	_ = db.Users.Query().Where(orm.NotInSub(Users.ID, orm.UnionAll(oneCol, oneCol)))
	_ = orm.Scalar[User, int64](orm.UnionAll(oneCol, oneCol))

	// Ordering a set operation by its own output columns.
	aliased := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"), func(v int64) int64 { return v })).From(usersSource)
	byID := orm.Named("id", orm.Of(Users.ID))
	_ = orm.UnionAll(aliased, aliased).OrderBy(byID.Desc()).Limit(10)
	_ = orm.UnionAll(aliased, aliased).OrderBy(byID.Asc(), byID.Desc())
	// And the builders that were value subqueries before still are.
	_ = orm.Scalar[User, int64](oneCol)
	_ = db.Users.Query().Where(orm.InSub(Users.ID, oneCol))
	_ = db.Users.Query().Where(orm.InSub(Users.ID, db.Users.Query()))

	_ = db.Users.Query().
		Where(orm.Expr[User]("score > $1", 100)).
		Offset(10).
		ForUpdate().
		Clone()

	// M10.1: a projection over the entity's own columns, with a nullable one
	// bound to a pointer and a raw expression stating its own result type.
	summary := orm.Project3(
		Users.ID, Users.Email, Users.Nickname,
		func(id int64, email string, nick *string) User { return User{ID: id, Email: email} },
	)
	_ = orm.Select(db.Users, summary).
		Where(Users.Age.Gt(18)).
		OrderBy(Users.CreatedAt.Desc()).
		Distinct().
		Limit(10).
		Offset(2).
		Clone()
	_ = orm.SelectFrom(db.Users, manager.Source(), summary)
	_ = orm.Project1(orm.RawValue[User, *string]("nickname"), func(s *string) *string { return s })
	_ = orm.Project2(Users.ID.As("user_id"), Users.Nickname.As("nick"),
		func(id int64, nick *string) int64 { return id })

	// M10.2: aggregates are selectable like anything else, and their result
	// types are PostgreSQL's rather than the input column's.
	stats := orm.Project4(
		Users.ManagerID,
		orm.Count[User]().As("n"),
		orm.SumInt32(Users.Age),
		orm.Min(Users.CreatedAt),
		func(mgr *int64, n int64, total *int64, first *time.Time) int64 { return n },
	)
	_ = orm.Select(db.Users, stats).
		GroupBy(Users.ManagerID).
		Having(orm.Count[User]().Gt(1), orm.SumInt32(Users.Age).IsNotNull()).
		OrderBy(Users.ManagerID.Asc())
	_ = orm.Count[User]().Filter(Users.Age.Gt(18)).Distinct()
	_ = orm.CountOf(Users.Nickname)
	_ = orm.AvgInt32[User, float64](Users.Age)

	// M10.3: expression assignments, RETURNING, and a richer conflict clause.
	_ = db.Users.Update().
		Set(
			Users.Age.SetExpr(Users.Age.Add(1)),
			Users.Nickname.SetExpr(orm.Nullable(Users.Email)),
			Users.ManagerID.SetExpr(orm.Nullable(Users.ID.Value())),
		).
		Where(Users.ID.Eq(1))
	_ = orm.UpdateReturning(
		db.Users.Update().Set(Users.Age.Set(1)).Where(Users.ID.Eq(1)), summary)
	_ = orm.UpdateReturningEntity(db.Users.Update().Set(Users.Age.Set(1)).All())
	_ = orm.DeleteReturning(db.Users.Delete().Where(Users.ID.Eq(1)), summary)
	_ = orm.DeleteReturningEntity(db.Users.Delete().All())
	_ = orm.OnConflict(Users.Email).
		Where(Users.Age.Gt(0)).
		DoUpdateSet(
			Users.Nickname.SetExpr(orm.Excluded(Users.Nickname)),
			Users.Age.SetExpr(Users.Age.Add(1)),
		)
	_ = db.Users.QueryFrom(manager.Source())

	// M4: the write API.
	_ = Users.Age.Set(18)
	_ = Users.Email.Set("a@example.com")
	_ = Users.Nickname.Set("alex")
	_ = Users.Nickname.SetNull()
	_ = Users.ManagerID.SetNull()
	_ = orm.Default(Users.CreatedAt, Users.Nickname)
	_ = orm.OnConflict(Users.Email).DoNothing()
	_ = orm.OnConflict(Users.Email, Users.Age).DoUpdate(Users.Nickname, Users.Age)

	_ = db.Users.Update().
		Set(Users.Age.Set(18), Users.Nickname.SetNull()).
		Where(Users.ID.Eq(1))
	_ = db.Users.Update().Set(Users.Age.Set(18)).All()
	_ = db.Users.Delete().Where(Users.ID.Eq(1))
	_ = db.Users.Delete().All()
}`
	if out, err := buildCase(t, dir, body); err != nil {
		t.Fatalf("valid usage did not compile:\n%s", out)
	}
}

// compileFailModule prepares one throwaway module the cases are built in.
//
// Building is slow, so the module — and the download of nothing, since the
// replace directive points at this tree — is set up once and every case reuses
// it, varying only the file under test.
func compileFailModule(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	dir := t.TempDir()

	// The module file is this one with its module path swapped, so the case
	// inherits every requirement the runtime already has — pgx and what pgx
	// needs — without a resolution step that could reach the network.
	ownMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	mod := strings.Replace(string(ownMod), "module github.com/AlexAli29/orm", "module ormcompilefail", 1) +
		"\nrequire github.com/AlexAli29/orm v0.0.0\n\nreplace github.com/AlexAli29/orm => " + root + "\n"
	write(t, filepath.Join(dir, "go.mod"), mod)

	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("reading go.sum: %v", err)
	}
	write(t, filepath.Join(dir, "go.sum"), string(sum))
	write(t, filepath.Join(dir, "main.go"), preamble)
	return dir
}

// buildOnce serialises the builds, which share one module directory.
var buildOnce sync.Mutex

// The preamble has to compile on its own. If it did not, every case above would
// fail to build for a reason that has nothing to do with what it asserts, and a
// suite whose cases all "fail correctly" would be measuring nothing.
func TestCompileFails_thePreambleItselfCompiles(t *testing.T) {
	dir := compileFailModule(t)
	body := `func good() {
	_ = db.Users.Query().With(UsersPosts.Where(Posts.Published.Eq(true)).OrderBy(Posts.CreatedAt.Desc()).Limit(5))
	_ = db.Posts.Query().With(PostsAuthor.Where(Users.Age.Eq(int32(1))))
	_ = db.Users.Query().Where(UsersPosts.Any(Posts.Published.Eq(true)))
	_ = db.Users.Query().Where(UsersPosts.None())
	_ = db.Users.Query().With(UsersPosts.With(PostsComments))
	_ = db.Users.Query().With(UsersPosts.Limit(5).With(PostsComments.Limit(10)))
	_ = db.Posts.Query().Where(PostsAuthor.Any(Users.Age.Eq(int32(1))))
	var l orm.Loader[User] = UsersPosts
	_ = l
}`
	if out, err := buildCase(t, dir, body); err != nil {
		t.Fatalf("the preamble does not compile, so every case above proves nothing:\n%s", out)
	}
}

// buildCase writes body beside the preamble and compiles the package.
func buildCase(t *testing.T, dir, body string) (string, error) {
	t.Helper()
	buildOnce.Lock()
	defer buildOnce.Unlock()

	path := filepath.Join(dir, "case.go")
	write(t, path, "package main\n\nimport (\n\t\"time\"\n\n\t\"github.com/AlexAli29/orm\"\n)\n\nvar _ = time.Now\nvar _ = orm.And[User]\n\n"+body+"\n")
	defer func() { _ = os.Remove(path) }()

	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", os.DevNull, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
