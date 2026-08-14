package uuidcompat_test

import (
	"strings"
	"testing"

	"example.com/uuidcompat/domain"
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
)

// Source-induced nullability, for uuid.
//
// orders.id is uuid NOT NULL. Read through a LEFT JOIN it can still be SQL
// NULL, because the join invents a row in which the whole right-hand source is
// absent. That is a property of the query and not of the column, and uuid has
// to obey it exactly as any other type does.
//
// The reason it needs a uuid test of its own rather than an int one is the zero
// value. uuid.UUID is an array, not a pointer: uuid.Nil is a perfectly ordinary
// value that a column may legitimately hold, and it is also what a Go uuid.UUID
// looks like when nothing was written into it. If the nullable read ever
// collapsed NULL onto the zero value, an int64 fixture would notice — 0 is
// conspicuous — and a uuid fixture that only ever stored random values would
// not. So the data below contains all three cases at once.

// joinRow is what one row of the outer join reads back as.
type joinRow struct {
	userID  uuid.UUID
	orderID *uuid.UUID
}

// leftJoinFixture writes the three rows the outer join has to tell apart and
// returns their user ids in the order matched / unmatched / zero.
func leftJoinFixture(t *testing.T, db *domain.DB) (matched, unmatched, zero uuid.UUID, matchedOrder uuid.UUID) {
	t.Helper()
	matched, unmatched, zero = uuid.New(), uuid.New(), uuid.New()
	matchedOrder = uuid.New()

	for _, u := range []domain.User{
		{ID: matched, Email: "matched@example.com", Tags: []uuid.UUID{}},
		{ID: unmatched, Email: "unmatched@example.com", Tags: []uuid.UUID{}},
		{ID: zero, Email: "zero@example.com", Tags: []uuid.UUID{}},
	} {
		if _, err := db.Users.Insert(t.Context(), u); err != nil {
			t.Fatalf("inserting user: %v", err)
		}
	}
	// The unmatched user deliberately gets no order at all.
	for _, o := range []domain.Order{
		{ID: matchedOrder, UserID: matched, Label: "ordinary"},
		// uuid.Nil is stored as a value. Nothing about it is missing.
		{ID: uuid.Nil, UserID: zero, Label: "zero"},
	} {
		if _, err := db.Orders.Insert(t.Context(), o); err != nil {
			t.Fatalf("inserting order: %v", err)
		}
	}
	return matched, unmatched, zero, matchedOrder
}

// The nullable side reads through Opt, and all three rows come back distinct.
func TestUUID_leftJoinSourceInducedNullability(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	matched, unmatched, zero, matchedOrder := leftJoinFixture(t, db)

	shape := orm.Project2(
		orm.Of(domain.Users.ID),
		// Opt, not Of: orders.id is NOT NULL and this join can still produce no
		// order at all.
		orm.Opt(domain.Orders.ID),
		func(u uuid.UUID, o *uuid.UUID) joinRow { return joinRow{userID: u, orderID: o} },
	)

	rows, err := orm.Compose(pool, shape).
		From(domain.Users.Source()).
		LeftJoin(domain.Orders.Source(),
			orm.Eq(domain.Orders.UserID, domain.Users.ID)).
		All(t.Context())
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("read %d rows, want 3", len(rows))
	}

	got := map[uuid.UUID]*uuid.UUID{}
	for _, r := range rows {
		got[r.userID] = r.orderID
	}

	// Unmatched: the source was absent, so the value is nil.
	if o, ok := got[unmatched]; !ok {
		t.Fatal("the unmatched user produced no row; a LEFT JOIN must keep it")
	} else if o != nil {
		t.Errorf("the unmatched row read %v, want nil: no order exists for it", *o)
	}

	// Matched, ordinary value: present, and exactly what was written.
	if o := got[matched]; o == nil {
		t.Error("the matched row read nil; an order exists for it")
	} else if *o != matchedOrder {
		t.Errorf("the matched row read %v, want %v", *o, matchedOrder)
	}

	// Matched, zero value: present, and still the zero UUID. This is the pair
	// that a collapsed representation would make indistinguishable.
	if o := got[zero]; o == nil {
		t.Error("the row whose order id is uuid.Nil read nil: the zero UUID is a " +
			"value, and reading it as absent loses the difference between a row " +
			"that has no order and a row whose order is identified by zero")
	} else if *o != uuid.Nil {
		t.Errorf("the zero row read %v, want %v", *o, uuid.Nil)
	}
}

// The two are distinguishable in the same result, which is the claim rather
// than either half of it.
func TestUUID_zeroUUIDAndJoinNullAreDifferentAnswers(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	_, unmatched, zero, _ := leftJoinFixture(t, db)

	shape := orm.Project2(
		orm.Of(domain.Users.ID),
		orm.Opt(domain.Orders.ID),
		func(u uuid.UUID, o *uuid.UUID) joinRow { return joinRow{userID: u, orderID: o} },
	)
	rows, err := orm.Compose(pool, shape).
		From(domain.Users.Source()).
		LeftJoin(domain.Orders.Source(),
			orm.Eq(domain.Orders.UserID, domain.Users.ID)).
		Where(orm.Cond(domain.Users.ID.In(unmatched, zero))).
		OrderBy(orm.Of(domain.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("read %d rows, want 2", len(rows))
	}

	var nils, zeros int
	for _, r := range rows {
		switch {
		case r.orderID == nil:
			nils++
		case *r.orderID == uuid.Nil:
			zeros++
		}
	}
	if nils != 1 || zeros != 1 {
		t.Errorf("read %d absent and %d zero, want one of each: the zero UUID and "+
			"the missing row are different answers and must not merge", nils, zeros)
	}
}

// Reading the nullable side with Of is refused.
//
// Of keeps the column's own type, and orders.id is NOT NULL, so the result
// would be uuid.UUID — a Go type with no way to say "there was no order". The
// refusal is the point: outer-join nullability dominates the constraint, and a
// select list that ignores it would fail as a scan error at some later row, or
// worse, not fail at all.
func TestUUID_outerJoinNullabilityDominatesTheColumnConstraint(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	leftJoinFixture(t, db)

	unsafe := orm.Project2(
		orm.Of(domain.Users.ID),
		// Of, on a source this statement brings in with LEFT JOIN.
		orm.Of(domain.Orders.ID),
		func(u, o uuid.UUID) joinRow { return joinRow{userID: u, orderID: &o} },
	)
	_, err := orm.Compose(pool, unsafe).
		From(domain.Users.Source()).
		LeftJoin(domain.Orders.Source(),
			orm.Eq(domain.Orders.UserID, domain.Users.ID)).
		All(t.Context())
	if err == nil {
		t.Fatal("reading a NOT NULL uuid through an outer join with Of was allowed; " +
			"the value can be SQL NULL and uuid.UUID cannot hold that")
	}
	// The refusal has to be the nullability one. Any other error passing this
	// test would report the property proven by an unrelated failure.
	for _, want := range []string{"public.orders", "outer join", "cannot hold NULL", "Opt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it may not be the "+
				"nullability refusal at all:\n%v", want, err)
		}
	}
}
