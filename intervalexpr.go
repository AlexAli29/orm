package orm

import (
	"time"

	"github.com/AlexAli29/orm/internal/expr"
)

// Interval arithmetic.
//
// Only the operations PostgreSQL states plainly are here, and each is typed
// with the result PostgreSQL actually produces rather than the one that would
// be tidy:
//
//	timestamptz + interval   timestamptz
//	timestamp   + interval   timestamp
//	date        + interval   timestamp        not date
//	interval    + interval   interval
//	interval    - interval   interval
//	interval    * numeric    interval
//
// The third line is the one that surprises people, and it is why this package
// does not try to preserve "date-ness": adding an interval to a date promotes it
// to a timestamp, because the interval may carry a time component the date
// cannot hold. Go sees time.Time on both sides, so the promotion costs nothing
// at the Go level and hiding it would only make the SQL harder to predict.
//
// There is no calendar DSL and there will not be one. Adding a month is
// PostgreSQL's job, it gets it right for 31 January, and reimplementing it in Go
// would produce a second set of rules to keep in agreement with the first.
//
// Each operation comes in two forms. The plain one takes operands that cannot be
// NULL and produces a result that cannot be NULL; the Null one takes [Optional]
// operands — a nullable column, or anything read through an outer join — and
// produces a nullable result, because in SQL an operator with a NULL operand is
// NULL.

// AddInterval builds timestamp + interval.
//
// It covers date + interval too, whose result PostgreSQL types as a timestamp
// rather than a date.
func AddInterval[A, B any](t Selectable[A, time.Time], iv Selectable[B, Interval]) Expression[time.Time, *time.Time] {
	return timeExpr("+", t.selectItem().Node, iv.selectItem().Node)
}

// AddIntervalNull is [AddInterval] where either side may be NULL.
func AddIntervalNull[A, B any](t Optional[A, *time.Time], iv Optional[B, *Interval]) Expression[*time.Time, *time.Time] {
	return nullTimeExpr("+", t.optItem().Node, iv.optItem().Node)
}

// SubInterval builds timestamp - interval.
func SubInterval[A, B any](t Selectable[A, time.Time], iv Selectable[B, Interval]) Expression[time.Time, *time.Time] {
	return timeExpr("-", t.selectItem().Node, iv.selectItem().Node)
}

// SubIntervalNull is [SubInterval] where either side may be NULL.
func SubIntervalNull[A, B any](t Optional[A, *time.Time], iv Optional[B, *Interval]) Expression[*time.Time, *time.Time] {
	return nullTimeExpr("-", t.optItem().Node, iv.optItem().Node)
}

// IntervalPlus builds interval + interval.
//
// PostgreSQL adds the three components independently — months to months, days
// to days, microseconds to microseconds — and does not normalise between them,
// which is the whole reason they are stored apart.
func IntervalPlus[A, B any](a Selectable[A, Interval], b Selectable[B, Interval]) Expression[Interval, *Interval] {
	return ivExpr("+", a.selectItem().Node, b.selectItem().Node)
}

// IntervalPlusNull is [IntervalPlus] where either side may be NULL.
func IntervalPlusNull[A, B any](a Optional[A, *Interval], b Optional[B, *Interval]) Expression[*Interval, *Interval] {
	return nullIvExpr("+", a.optItem().Node, b.optItem().Node)
}

// IntervalMinus builds interval - interval.
func IntervalMinus[A, B any](a Selectable[A, Interval], b Selectable[B, Interval]) Expression[Interval, *Interval] {
	return ivExpr("-", a.selectItem().Node, b.selectItem().Node)
}

// IntervalMinusNull is [IntervalMinus] where either side may be NULL.
func IntervalMinusNull[A, B any](a Optional[A, *Interval], b Optional[B, *Interval]) Expression[*Interval, *Interval] {
	return nullIvExpr("-", a.optItem().Node, b.optItem().Node)
}

// IntervalTimes builds interval * factor, scaling all three components.
//
// A fractional factor is allowed and PostgreSQL spills the remainder downwards:
// half of one month is fifteen days, and half of one day is twelve hours. That
// is an approximation the server makes, not one this package makes for it.
func IntervalTimes[A, B any](iv Selectable[A, Interval], factor Selectable[B, float64]) Expression[Interval, *Interval] {
	return ivExpr("*", iv.selectItem().Node, factor.selectItem().Node)
}

// IntervalTimesNull is [IntervalTimes] where either side may be NULL.
func IntervalTimesNull[A, B any](iv Optional[A, *Interval], factor Optional[B, *float64]) Expression[*Interval, *Interval] {
	return nullIvExpr("*", iv.optItem().Node, factor.optItem().Node)
}

func timeExpr(op string, l, r expr.Node) Expression[time.Time, *time.Time] {
	return Expression[time.Time, *time.Time]{node: expr.Infix{Op: op, Left: l, Right: r}}
}

func nullTimeExpr(op string, l, r expr.Node) Expression[*time.Time, *time.Time] {
	return Expression[*time.Time, *time.Time]{node: expr.Infix{Op: op, Left: l, Right: r}, nullSafe: true}
}

func ivExpr(op string, l, r expr.Node) Expression[Interval, *Interval] {
	return Expression[Interval, *Interval]{node: expr.Infix{Op: op, Left: l, Right: r}}
}

func nullIvExpr(op string, l, r expr.Node) Expression[*Interval, *Interval] {
	return Expression[*Interval, *Interval]{node: expr.Infix{Op: op, Left: l, Right: r}, nullSafe: true}
}
