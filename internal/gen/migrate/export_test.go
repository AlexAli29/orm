package migrate

import "context"

// Test hooks.
//
// The advisory key is deliberately not exported: it is an implementation
// detail of how two migrators on one database serialise, and nothing outside
// this package has a reason to take the lock. A test does, because holding it
// by hand is the only way to prove the waiting behaviour is real.
func LockKeyForTest() int64 { return lockKey }

// ExecOperationForTest runs one operation the way a migration does, so that
// tests can prove the execution path rather than the functions underneath it.
func ExecOperationForTest(ctx context.Context, ex SQLRunner, op Operation) error {
	return execOperation(ctx, ex, op)
}
