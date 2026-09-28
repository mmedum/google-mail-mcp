package service

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/mmedum/google-mail-mcp/v2/internal/gapi"
)

// fanOutLimit is how many per-row reads one listing has in flight. It
// keeps a page of twenty from taking twenty round trips in a row without
// sending Google twenty requests at once. The client keeps as many idle
// connections, so the reads reuse them.
const fanOutLimit = gapi.MaxInFlight

// fanOut calls read for each of n rows, at most fanOutLimit at a time,
// and returns the results in row order.
//
// The error returned is the lowest-numbered row's, whichever failed
// first in time, so the same failure gives the same answer on every run.
// Rows are started in order, and none is started after a lower-numbered
// row has failed; every row before a failure still runs.
func fanOut[T any](ctx context.Context, n int, read func(ctx context.Context, i int) (T, error)) ([]T, error) {
	out := make([]T, n)
	errs := make([]error, n)
	var firstFailed atomic.Int64
	firstFailed.Store(int64(n))
	sem := make(chan struct{}, fanOutLimit)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		if int64(i) > firstFailed.Load() {
			<-sem
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			v, err := read(ctx, i)
			if err != nil {
				errs[i] = err
				for {
					cur := firstFailed.Load()
					if int64(i) >= cur || firstFailed.CompareAndSwap(cur, int64(i)) {
						break
					}
				}
				return
			}
			out[i] = v
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
