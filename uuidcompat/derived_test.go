package uuidcompat_test

import (
	"testing"

	"example.com/uuidcompat/domain"
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
)

// uuid across a derivation boundary.
//
// A derived table and a CTE both republish a column under a name the query
// declares, and the type of what comes back is decided by that declaration
// rather than by the physical column. That is one more place a configured
// mapping could be lost — the output could arrive as text, or as []byte, or as
// something scanned through an interface — and the only way to know it is not
// is to read it back as uuid.UUID and compare against what was written.

// A derived table republishes a uuid column, and a predicate over the derived
// column is still a typed uuid comparison.
func TestUUID_survivesADerivedTable(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	two, one, _ := projectionFixture(t, db)

	userID := orm.Named("user_id", orm.Of(domain.Orders.UserID))
	orderCount := orm.Named("order_count", orm.Count[orm.Composed]())

	perUser := orm.Sub("per_user", orm.Rows(userID, orderCount).
		From(domain.Orders.Source()).
		GroupBy(orm.Of(domain.Orders.UserID)))

	shape := orm.Project2(
		orm.Ref(perUser, userID),
		orm.Ref(perUser, orderCount),
		func(id uuid.UUID, n int64) tally { return tally{userID: id, orders: n} },
	)

	rows, err := orm.Compose(pool, shape).
		From(perUser).
		// The comparison is against the derived column, and it takes a
		// uuid.UUID because the declaration said the column is one.
		Where(orm.Ref(perUser, userID).Eq(two)).
		All(t.Context())
	if err != nil {
		t.Fatalf("querying the derived table: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read %d rows, want 1", len(rows))
	}
	if rows[0].userID != two {
		t.Errorf("the derived uuid came back as %v, want %v", rows[0].userID, two)
	}
	if rows[0].orders != 2 {
		t.Errorf("the derived count is %d, want 2", rows[0].orders)
	}
	_ = one
}

// The same across a CTE, joined back to the table it came from.
func TestUUID_survivesACTEAndJoinsBackOnIt(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	two, one, none := projectionFixture(t, db)

	busyID := orm.Named("user_id", orm.Of(domain.Orders.UserID))
	busy := orm.CTE("busy_users", orm.Rows(busyID).
		From(domain.Orders.Source()).
		GroupBy(orm.Of(domain.Orders.UserID)))

	type row struct {
		id    uuid.UUID
		email string
	}
	shape := orm.Project2(
		orm.Of(domain.Users.ID),
		orm.Of(domain.Users.Email),
		func(id uuid.UUID, email string) row { return row{id: id, email: email} },
	)

	rows, err := orm.Compose(pool, shape).
		With(busy).
		From(domain.Users.Source()).
		// Joining a table's uuid column to a CTE's uuid column: both sides had
		// to keep the type for this to be expressible at all.
		Join(busy, orm.Eq(orm.Ref(busy, busyID), orm.Of(domain.Users.ID))).
		OrderBy(orm.Of(domain.Users.Email).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("querying through the CTE: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("read %d rows, want 2 (the users that have orders)", len(rows))
	}
	got := map[uuid.UUID]bool{}
	for _, r := range rows {
		got[r.id] = true
	}
	if !got[two] || !got[one] {
		t.Errorf("the join through the CTE lost a user: got %v", got)
	}
	if got[none] {
		t.Error("a user with no orders came back through a join on the CTE")
	}
}
