// Package broken does not type-check. Reconciling it would mean comparing the
// schema against types the compiler never resolved, so the scanner refuses
// rather than reporting findings derived from guesses.
//
// The go tool ignores directories named testdata, so this never reaches a
// build of the module itself.
package broken

//orm:table users
type User struct {
	ID   int64
	Name NoSuchType
}
