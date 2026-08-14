package orm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Structural fingerprints.
//
// Two executions of the same query with different values are the same query for
// almost every purpose worth having: grouping slow statements, attaching an
// identity to a trace, keeping a history of what a shape's plan used to look
// like. A fingerprint is that identity.
//
// It is computed from the SQL the statement compiles to, with the values left
// out — which is possible without a SQL parser because the values were never in
// the SQL. Every value this package sends is a bind parameter, so the compiled
// statement is already the shape: WHERE "email" = $1 says nothing about who.
// That is not a convenience here, it is the reason this works at all.
//
// What a fingerprint is not:
//
//	a cache key       two statements with the same shape return different rows
//	a secret          it is derived from structure, and structure is not secret
//	a query id        PostgreSQL's own compute_query_id is a different number
//	                  computed differently, and neither replaces the other
//
// The digest is SHA-256 over a canonical byte string. Go's map hash is seeded
// per process and would give a different answer in every run, which is the one
// thing a fingerprint must not do.

// FingerprintVersion is the version of the algorithm below.
//
// It is part of every fingerprint's text so that a change to what counts as
// structure is detectable rather than silent: a v1 and a v2 fingerprint of the
// same query are visibly different things rather than a mysterious change in
// grouping.
const FingerprintVersion = 1

// Fingerprint identifies a statement's structure.
//
// The zero value is not a fingerprint of anything, and reports so.
type Fingerprint struct {
	// Version is the algorithm that produced it.
	Version uint8
	// Digest is the SHA-256 of the canonical form.
	Digest [32]byte
}

// String renders the fingerprint as "v1:" and the first sixteen bytes of the
// digest in hex.
//
// Sixteen bytes is 128 bits, which is more than enough to keep a workload's
// statement shapes apart and short enough to read in a log line. [Fingerprint.Full]
// returns the whole digest.
func (f Fingerprint) String() string {
	if f.Version == 0 {
		return ""
	}
	return fmt.Sprintf("v%d:%s", f.Version, hex.EncodeToString(f.Digest[:16]))
}

// Full renders the fingerprint with the complete digest.
func (f Fingerprint) Full() string {
	if f.Version == 0 {
		return ""
	}
	return fmt.Sprintf("v%d:%s", f.Version, hex.EncodeToString(f.Digest[:]))
}

// IsZero reports whether this is the zero fingerprint, which identifies nothing.
func (f Fingerprint) IsZero() bool { return f.Version == 0 }

// Equal reports whether two fingerprints identify the same structure.
func (f Fingerprint) Equal(other Fingerprint) bool {
	return f.Version == other.Version && f.Digest == other.Digest
}

// fingerprintOf computes the fingerprint of a compiled statement.
//
// kind separates statements that could otherwise compile to the same text —
// a COPY has no SQL at all — and the SQL is used as written, because the
// writer is deterministic: the same query structure produces the same bytes on
// every run and in every process.
//
// Nothing is normalised beyond this, and the reason is always the same: the
// point of grouping is that the group shares a plan.
//
// Sorting the branches of an AND would group statements the writer renders
// differently. Collapsing IN ($1, $2) and IN ($1, $2, $3) would group two
// statements PostgreSQL costs differently. And LIMIT and OFFSET are rendered as
// literals by this package's writer, so LIMIT 10 and LIMIT 1000000 are
// different statements here — which is right, because they get different plans:
// a small limit is what makes the planner prefer an index it would otherwise
// ignore.
//
// Grouping by "looks similar" is worth less than grouping by "is the same
// statement", and every one of these is a place where the two diverge.
func fingerprintOf(kind string, sql string) Fingerprint {
	h := sha256.New()
	// The version is hashed in as well as printed, so that two algorithm
	// versions cannot collide even in the truncated form.
	fmt.Fprintf(h, "orm-fingerprint\x00v%d\x00%s\x00", FingerprintVersion, kind)
	h.Write([]byte(sql))

	var f Fingerprint
	f.Version = FingerprintVersion
	copy(f.Digest[:], h.Sum(nil))
	return f
}

// FingerprintOf returns the fingerprint of any statement this package compiles.
//
// A statement that does not compile has no fingerprint, and the error is the
// one the statement would have failed with.
func FingerprintOf(s Statement) (Fingerprint, error) {
	if s == nil {
		return Fingerprint{}, fmt.Errorf("orm: no statement to fingerprint")
	}
	sql, _, err := s.SQL()
	if err != nil {
		return Fingerprint{}, err
	}
	return fingerprintOf(statementKind(s), sql), nil
}

// statementKind names what sort of statement something is, so that two
// different builders producing the same text are still told apart.
//
// It is derived from the compiled SQL's first word rather than from the Go type,
// because the Go type is generic and the first word is what PostgreSQL will act
// on. A statement whose first word is not recognised is "other", which groups
// nothing wrongly.
func statementKind(s Statement) string {
	sql, _, err := s.SQL()
	if err != nil {
		return "invalid"
	}
	return sqlKind(sql)
}

func sqlKind(sql string) string {
	trimmed := strings.TrimLeft(sql, " \t\r\n(")
	// A statement starting with WITH is named by what it eventually does, which
	// needs more than the first word; the first word is still the honest answer
	// for grouping, because a WITH ... SELECT and a WITH ... UPDATE compile to
	// visibly different text anyway.
	for _, kind := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "WITH", "COPY", "EXPLAIN"} {
		if len(trimmed) >= len(kind) && strings.EqualFold(trimmed[:len(kind)], kind) {
			return strings.ToLower(kind)
		}
	}
	return "other"
}

// The convenience methods.
//
// Each compiles the statement and fingerprints it. A statement that will not
// compile has no fingerprint, and the error says why — which is the same error
// running it would have produced.

// Fingerprint identifies this query's structure.
func (q *Query[E]) Fingerprint() (Fingerprint, error) { return FingerprintOf(q) }

// Fingerprint identifies this query's structure.
func (q *SelectQuery[E, R]) Fingerprint() (Fingerprint, error) { return FingerprintOf(q) }

// Fingerprint identifies this query's structure.
func (q *ComposedQuery[R]) Fingerprint() (Fingerprint, error) { return FingerprintOf(q) }

// Fingerprint identifies this statement's structure.
func (u *Update[E]) Fingerprint() (Fingerprint, error) { return FingerprintOf(u) }

// Fingerprint identifies this statement's structure.
func (d *Delete[E]) Fingerprint() (Fingerprint, error) { return FingerprintOf(d) }

// Fingerprint identifies this statement's structure.
//
// A raw statement's structure is the SQL the caller wrote. Nothing normalises
// it, so two spellings of the same query — a different alias, different
// whitespace — are different structures here. That is the honest answer for
// text this package did not build, and building a SQL parser to do better would
// be building the thing this package exists not to have.
func (q *RawQuery[E]) Fingerprint() (Fingerprint, error) { return FingerprintOf(q) }

// CopyFingerprint identifies a COPY by what it copies.
//
// A COPY has no statement to fingerprint: pgx sends the protocol's copy message
// rather than SQL, so there is no text to take the shape of. Its identity is
// therefore built from the parts that decide what the operation does — the
// table and the columns, in the order they are sent — because a COPY into the
// same table with the columns in a different order is a different operation on
// the wire.
//
// The rows are not part of it, for the same reason values are not part of any
// other fingerprint.
func CopyFingerprint(schema, table string, columns []string) Fingerprint {
	var b strings.Builder
	b.WriteString(schema)
	b.WriteByte('.')
	b.WriteString(table)
	for _, c := range columns {
		b.WriteByte('\x00')
		b.WriteString(c)
	}
	return fingerprintOf("copy", b.String())
}
