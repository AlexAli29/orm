package observe

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Classifying an error without repeating it.
//
// This is the one place this package depends on pgx, and it is for a type
// assertion: a PostgreSQL error carries a SQLSTATE and the names of the objects
// involved, and those are worth reporting where the message is not. The message
// is not repeated here because it quotes the data — "Key (email)=(a@b.c) already
// exists" is the server being helpful and a telemetry pipeline being handed an
// address.

// Classify summarises an error into the parts that are structure rather than
// data.
//
// A tracer that records [ErrorInfo] rather than the error itself records that a
// unique violation happened on users_email_key, and not which email it was.
func Classify(err error) ErrorInfo {
	if err == nil {
		return ErrorInfo{}
	}
	info := ErrorInfo{Failed: true, Kind: "other"}

	if errors.Is(err, context.Canceled) {
		info.Kind = "cancelled"
		return info
	}
	if errors.Is(err, context.DeadlineExceeded) {
		info.Kind = "deadline_exceeded"
		return info
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return info
	}
	info.SQLState = pgErr.Code
	info.Table = pgErr.TableName
	info.Constraint = pgErr.ConstraintName
	info.Column = pgErr.ColumnName
	info.Kind = kindOf(pgErr.Code)
	return info
}

// kindOf names the SQLSTATE codes worth telling apart in a dashboard.
//
// The rest fall back to their class, which is the first two characters — a
// number rather than a guess, and enough to separate a syntax error from a
// connection failure without inventing a taxonomy.
func kindOf(code string) string {
	switch code {
	case "23505":
		return "unique_violation"
	case "23503":
		return "foreign_key_violation"
	case "23502":
		return "not_null_violation"
	case "23514":
		return "check_violation"
	case "40001":
		return "serialization_failure"
	case "40P01":
		return "deadlock_detected"
	case "55P03":
		return "lock_not_available"
	case "57014":
		return "query_canceled"
	case "42P01":
		return "undefined_table"
	case "42703":
		return "undefined_column"
	case "42601":
		return "syntax_error"
	}
	if len(code) >= 2 {
		return "class_" + code[:2]
	}
	return "other"
}
