package migrator

import (
	"context"
	"fmt"
	"sync"

	"github.com/rustfs/migration/internal/config"
)

// spoolBudget reserves the entire temporary file before downloading, so no
// collection of partial files can deadlock waiting for space to finish.
type spoolBudget struct {
	mu          sync.Mutex
	limit, used int64
	changed     chan struct{}
}

func newSpoolBudget(limit int64) *spoolBudget {
	if limit == 0 {
		limit = config.DefaultSpoolMaxBytes
	}
	return &spoolBudget{limit: limit, changed: make(chan struct{})}
}

func (b *spoolBudget) acquire(ctx context.Context, size int64) (func(), error) {
	if size < 0 || size > b.limit {
		return nil, fmt.Errorf("spool reservation %d exceeds spool_max_bytes %d; increase budget or reduce part size", size, b.limit)
	}
	for {
		b.mu.Lock()
		if err := ctx.Err(); err != nil {
			b.mu.Unlock()
			return nil, err
		}
		if size <= b.limit-b.used {
			b.used += size
			b.mu.Unlock()
			return func() {
				b.mu.Lock()
				b.used -= size
				close(b.changed)
				b.changed = make(chan struct{})
				b.mu.Unlock()
			}, nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
