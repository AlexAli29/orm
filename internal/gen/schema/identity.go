package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Three identities, and why there have to be three.
//
// A view's definition is compared for three different reasons, on three
// different occasions, and no single representation answers all of them. The
// implementation that tried would be wrong in whichever direction was least
// visible, so they are named apart here.
//
// # SourceIdentity — what the project declares
//
// A fingerprint of the developer's own SQL. It never touches a server, so it is
// the same on every machine and against every PostgreSQL version, which is what
// makes it the only one of the three that may enter orm.lock. It answers:
// has the declaration changed since the lock was written?
//
// # ServerCanonical — what one server says a definition is
//
// pg_get_viewdef output. PostgreSQL parses a definition, stores the parsed
// query and deparses it on request, so this sees through formatting and
// comments completely — and is not portable, because the deparser changes
// between majors. PostgreSQL 16 stopped qualifying columns it does not need to,
// so one unchanged view reads differently on 15 and 16. It answers: does this
// database still hold the definition that was applied to it?
//
// # ActualDefinition — what the target database currently stores
//
// The same thing as ServerCanonical, read now rather than at apply time. Drift
// is the difference between the two, and both come from the same server, so the
// deparser's version cancels out and the comparison means something.
//
// None of these is SQL semantic equivalence. Nothing here proves that two
// differently written queries mean the same thing, and nothing here parses SQL.

// SourceIdentity is the portable identity of a declared definition.
//
// It is a fingerprint of the SQL as written. Reformatting a definition changes
// it, and that is a deliberate decision rather than an oversight: making it
// formatting-independent needs a tokenizer that knows a space inside a string
// literal is data and a space between keywords is not, and a tokenizer that is
// approximately right about PostgreSQL's grammar is worse than none — it would
// silently call two different definitions equal.
//
// The consequence is small and is documented where a user meets it: reindenting
// a definition changes the lock and produces a migration whose SQL PostgreSQL
// parses to the same query it already had. That is a harmless statement, and
// the alternative is a comparison nobody can trust.
//
// Formatting is invisible to drift detection, which is a different question
// asked against a server. See [Definition.SameOnServer].
type SourceIdentity string

// SourceIdentityOf fingerprints a declared definition.
//
// The prefix is a version, so that a future change of scheme is a visible
// change of identity rather than a silent reinterpretation of old locks.
func SourceIdentityOf(sql string) SourceIdentity {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sql)))
	return SourceIdentity("v1:" + hex.EncodeToString(sum[:16]))
}

// Identity returns the definition's portable source identity.
func (d Definition) Identity() SourceIdentity { return SourceIdentityOf(d.SQL) }

// SameOnServer reports whether two definitions read the same on one server.
//
// Both sides must be canonical text from the same PostgreSQL instance: one
// recorded when a definition was applied, the other read now. That is what
// makes the comparison meaningful — the deparser is the same, so formatting and
// comments are already gone and what is left is the parsed query.
//
// It is never a comparison between a project's own SQL and a server's
// reconstruction. Those two are almost never equal even when they mean exactly
// the same thing, and treating them as comparable is the mistake this method
// exists to make impossible: the parameters are canonical texts, and there is
// no overload taking source SQL.
func SameOnServer(recorded, actual string) bool {
	return strings.TrimSpace(recorded) == strings.TrimSpace(actual)
}
