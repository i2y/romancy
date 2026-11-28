package storage

import (
	"context"
	"time"
)

// releaseTimeout bounds a lock release that outlives its caller's context.
const releaseTimeout = 10 * time.Second

// ReleaseLockDetached releases an instance lock on ctx's values but not on
// its cancellation. A run cancelled at shutdown, or a request whose client
// went away, still hands the lock back instead of leaving the instance
// leased until the lease expires. The release is bounded, so an unreachable
// database cannot hold the caller.
func ReleaseLockDetached(ctx context.Context, s Storage, instanceID, holder string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	return s.ReleaseLock(ctx, instanceID, holder)
}
