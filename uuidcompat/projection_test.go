package uuidcompat_test

import (
	"testing"

	"example.com/uuidcompat/domain"
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
)

// uuid through a result shape that is not an entity.
//
// Reading a row into the struct the table was declared from proves the mapping
// once. A projection is a different path: the shape is built from column
// descriptors, the scan is generated from the shape rather than from the entity,
// and a type that survived one does not automatically survive the other.
//
// The grouped case also reads count(*) beside the uuid it groups by, so the
// int8 mapping the type-discovery fix restored is exercised by a uuid query
// rather than only by the regression fixture that found it.

type tally struct {
	userID uuid.UUID
	orders int64
}

// projectionFixture writes two users with two and one orders respectively, and
// a third with none, so a grouped count has something to be wrong about.
func projectionFixture(t *testing.T, db *domain.DB) (two, one, none uuid.UUID) {
	t.Helper()
	two, one, none = uuid.New(), uuid.New(), uuid.New()
	for _, u := range []domain.User{
		{ID: two, Email: "two@example.com", Tags: []uuid.UUID{}},
		{ID: one, Email: "one@example.com", Tags: []uuid.UUID{}},
		{ID: none, Email: "none@example.com", Tags: []uuid.UUID{}},
	} {
		if _, err := db.Users.Insert(t.Context(), u); err != nil {
			t.Fatalf("inserting user: %v", err)
		}
	}
	for _, o := range []domain.Order{
		{ID: uuid.New(), UserID: two, Label: "a"},
		{ID: uuid.New(), UserID: two, Label: "b"},
		{ID: uuid.New(), UserID: one, Label: "c"},
	} {
		if _, err := db.Orders.Insert(t.Context(), o); err != nil {
			t.Fatalf("inserting order: %v", err)
		}
	}
	return two, one, none
}

// A typed uuid projection scans exactly, and orders by the uuid column.
func TestUUID_typedProjectionAndOrdering(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	projectionFixture(t, db)

	type row struct {
		id    uuid.UUID
		email string
	}
	shape := orm.Project2(domain.Users.ID, domain.Users.Email,
		func(id uuid.UUID, email string) row { return row{id: id, email: email} })

	rows, err := orm.Select(db.Users, shape).
		// Ordering by a uuid column is PostgreSQL's ordering of uuid, which is
		// byte order and not the order the text spelling would give.
		OrderBy(domain.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("selecting: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("read %d rows, want 3", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1].id, rows[i].id
		if cur.String() < prev.String() {
			// String comparison agrees with byte order for the canonical
			// spelling, which makes it a usable independent check here.
			t.Errorf("row %d id %v sorts before row %d id %v", i, cur, i-1, prev)
		}
	}
	// The projected values are the stored ones, not a re-rendered spelling.
	seen := map[uuid.UUID]string{}
	for _, r := range rows {
		seen[r.id] = r.email
	}
	if len(seen) != 3 {
		t.Errorf("the projection produced %d distinct ids for 3 rows", len(seen))
	}
}

// GROUP BY a uuid column, with count(*) beside it.
func TestUUID_groupByUUIDWithCount(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	two, one, none := projectionFixture(t, db)

	shape := orm.Project2(
		domain.Orders.UserID,
		// count(*) is bigint, and the canonical mapping for bigint is int64.
		orm.Count[domain.Order](),
		func(id uuid.UUID, n int64) tally { return tally{userID: id, orders: n} },
	)
	rows, err := orm.Select(db.Orders, shape).
		GroupBy(domain.Orders.UserID).
		OrderBy(domain.Orders.UserID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("selecting: %v", err)
	}

	got := map[uuid.UUID]int64{}
	for _, r := range rows {
		got[r.userID] = r.orders
	}
	if got[two] != 2 {
		t.Errorf("the user with two orders counted %d", got[two])
	}
	if got[one] != 1 {
		t.Errorf("the user with one order counted %d", got[one])
	}
	// A user with no orders has no row in a GROUP BY over orders, which is
	// PostgreSQL's answer and not something to paper over.
	if _, ok := got[none]; ok {
		t.Errorf("the user with no orders appeared in a grouping over orders")
	}
}

// The grouped aggregate keeps its canonical Go type.
//
// The value is int64 because bigint maps to int64, and that is fixed by the
// declaration of the shape: if the aggregate ever came back as something else,
// this file would not compile. The runtime assertion is that the number is
// right, which a wrong-typed but compiling change could still get wrong.
func TestUUID_groupedAggregateIsTheCanonicalInt8Mapping(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	projectionFixture(t, db)

	shape := orm.Project1(orm.Count[domain.Order](), func(n int64) int64 { return n })
	totals, err := orm.Select(db.Orders, shape).All(t.Context())
	if err != nil {
		t.Fatalf("selecting: %v", err)
	}
	if len(totals) != 1 || totals[0] != 3 {
		t.Fatalf("count(*) = %v, want [3]", totals)
	}

	// And the same aggregate read off the materialized view, where the column
	// is a stored bigint that no table in this schema holds.
	if err := db.UserSummaries.Refresh(t.Context()); err != nil {
		t.Fatalf("refreshing: %v", err)
	}
	summaries, err := db.UserSummaries.Query().All(t.Context())
	if err != nil {
		t.Fatalf("reading the materialized view: %v", err)
	}
	var total int64
	for _, s := range summaries {
		total += s.Orders
	}
	if total != 3 {
		t.Errorf("the materialized view's bigint column totals %d, want 3", total)
	}
}
