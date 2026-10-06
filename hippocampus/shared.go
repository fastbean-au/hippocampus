package hippocampus

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"
)

// sharedWorkTimeout bounds work shared through a singleflight group once it no longer runs on any
// one caller's context. It is generous because the shared work is a store scan, which each store
// operation already bounds through storage.queryTimeoutSeconds; this only stops a scan outliving
// every reason to finish it.
const sharedWorkTimeout = 5 * time.Minute

// sharedCall runs fn once for every caller sharing key, as Group.Do does, but on a context detached
// from whichever caller arrived first (TODO-3 item 164). On the leader's context, one closed console
// tab cancelled the scan that every other viewer had joined, and they all failed with an error that
// was not theirs. Detached, the work belongs to the server, bounded by sharedWorkTimeout and still
// carrying the leader's values (its trace span), and each caller waits on its own context: one that
// gives up returns at once, leaving the work running for the rest.
func sharedCall(
	ctx context.Context,
	group *singleflight.Group,
	key string,
	fn func(context.Context) (any, error),
) (any, error) {
	results := group.DoChan(key, func() (any, error) {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedWorkTimeout)
		defer cancel()

		return fn(work)
	})

	select {

	case result := <-results:
		return result.Val, result.Err

	case <-ctx.Done():
		return nil, ctx.Err()

	}
}
