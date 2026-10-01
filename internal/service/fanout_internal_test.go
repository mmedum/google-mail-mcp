package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// No row is started after a lower-numbered row has failed. Row 0 fails
// at once while the other rows of the first window wait, so the window
// frees only when row 0 is done; the row after the window must then
// never be read. The wait ends early if it is.
func TestNoRowStartsAfterAFailure(t *testing.T) {
	const n = 3 * fanOutLimit
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	time.AfterFunc(100*time.Millisecond, free)

	var mu sync.Mutex
	var read []int
	_, err := fanOut(context.Background(), n, func(_ context.Context, i int) (int, error) {
		mu.Lock()
		read = append(read, i)
		mu.Unlock()
		if i == 0 {
			return 0, errors.New("row 0 failed")
		}
		if i >= fanOutLimit {
			free()
		}
		<-release
		return i, nil
	})
	if err == nil || err.Error() != "row 0 failed" {
		t.Fatalf("err = %v; want row 0's", err)
	}
	for _, i := range read {
		if i >= fanOutLimit {
			t.Errorf("rows read %v; want none from %d on, past row 0's failure", read, fanOutLimit)
			break
		}
	}
}
