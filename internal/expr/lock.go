package expr

import "fmt"

// Row locking.
//
// A locking clause tells PostgreSQL to hold the rows the statement returned
// until the transaction ends, and it is four decisions rather than one: how
// strong the lock is, what to do when somebody else already holds one, and
// which of the statement's sources to lock.
//
// M5 needed one of those — FOR UPDATE, on the root table — and a bool carried
// it. Public joins made the rest worth having, so the bool became this.

// LockStrength is how strongly a row is locked.
//
// The four are ordered weakest to strongest in PostgreSQL's own conflict table:
// KEY SHARE and SHARE only block writers, NO KEY UPDATE allows a concurrent
// foreign-key check, and UPDATE blocks everything. Choosing the weakest that
// answers the question is what keeps two workers from queueing behind each
// other for no reason.
type LockStrength uint8

// The locking strengths PostgreSQL offers. The zero value is no lock at all,
// which is what a statement without a locking clause has.
const (
	LockNone LockStrength = iota
	LockKeyShare
	LockShare
	LockNoKeyUpdate
	LockUpdate
)

var lockStrengthSQL = map[LockStrength]string{
	LockKeyShare:    "FOR KEY SHARE",
	LockShare:       "FOR SHARE",
	LockNoKeyUpdate: "FOR NO KEY UPDATE",
	LockUpdate:      "FOR UPDATE",
}

// String returns the clause's PostgreSQL spelling.
func (s LockStrength) String() string {
	if v, ok := lockStrengthSQL[s]; ok {
		return v
	}
	return "no lock"
}

// LockWait is what to do about a row somebody else has locked.
type LockWait uint8

// The three waiting policies. The default blocks until the other transaction
// ends, which is what a locking clause does when nothing else is said.
const (
	// LockWaitBlock waits for the other transaction. It is PostgreSQL's
	// default and writes nothing.
	LockWaitBlock LockWait = iota
	// LockNoWait fails immediately rather than waiting.
	LockNoWait
	// LockSkipLocked leaves the locked rows out of the result.
	LockSkipLocked
)

var lockWaitSQL = map[LockWait]string{
	LockNoWait:     " NOWAIT",
	LockSkipLocked: " SKIP LOCKED",
}

// Lock is a statement's locking clause.
type Lock struct {
	Strength LockStrength
	Wait     LockWait
	// Of names the sources to lock. Empty locks every source the statement
	// can lock, which is PostgreSQL's default.
	Of []*Source
}

// writeLock renders the clause, which PostgreSQL's grammar puts last — after
// ORDER BY, LIMIT and OFFSET.
func (w *writer) writeLock(l Lock, scope *Scope) {
	if l.Strength == LockNone {
		return
	}
	sql, ok := lockStrengthSQL[l.Strength]
	if !ok {
		w.fail(fmt.Errorf("cannot compile lock strength %d", l.Strength))
		return
	}
	w.b.WriteByte(' ')
	w.b.WriteString(sql)

	for i, src := range l.Of {
		if src == nil {
			w.fail(fmt.Errorf("a lock names no source"))
			return
		}
		// A source the statement does not select from cannot be locked, and
		// PostgreSQL says so in terms of a relation that is not there. Saying
		// it here names the occurrence instead.
		if !scope.Visible(src) {
			w.fail(&ScopeError{Source: src, Visible: scope.Sources()})
			return
		}
		if i == 0 {
			w.b.WriteString(" OF ")
		} else {
			w.b.WriteString(", ")
		}
		w.ident(src.Ref())
	}
	w.b.WriteString(lockWaitSQL[l.Wait])
}
