// Package outer imports [inner] and nothing else.
//
// It is the negative control for the boundary test's most important property:
// that the check reads the whole dependency graph rather than the direct
// imports. outer does not import the ORM. It depends on it, through one hop,
// which is exactly how a violation arrives in practice — nobody adds
// `import "github.com/AlexAli29/orm"` to a domain package; somebody adds a
// helper, and the helper has a dependency.
//
// A check written against direct imports finds nothing here. A check written
// against the full dependency set finds it, and the test asserts that it does.
package outer

import _ "example.com/hexagonal/control/inner"
