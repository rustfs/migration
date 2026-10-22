// Package ratelimit is a shared bytes-per-second token bucket.
//
// One Limiter is safe for concurrent use. Every successful read charges the
// real byte count against a single timeline, so N workers together stay near
// the configured rate instead of each getting their own copy of it.
// A nil Limiter or a non-positive rate means unlimited.
package ratelimit

import (
	"context"
	"io"
	"math"
	"sync"
	"time"
)

// Limiter paces bytes across every caller that shares it.
type Limiter struct {
	bps int64

	mu   sync.Mutex
	next time.Time // when the next unpaid byte may be consumed
}

// New returns a limiter of bytesPerSecond. Values <= 0 return nil (unlimited).
func New(bytesPerSecond int64) *Limiter {
	if bytesPerSecond <= 0 {
		return nil
	}
	return &Limiter{bps: bytesPerSecond}
}

// Limit reports the configured bytes per second. Nil is unlimited (0).
func (l *Limiter) Limit() int64 {
	if l == nil {
		return 0
	}
	return l.bps
}

// WaitN blocks until n bytes fit on the shared timeline, or until ctx ends.
// The wait is "until the previous bytes have been paid"; this call's own
// cost is what delays the next caller. Cancelling gives the unused tail
// back when this reservation is still the latest one.
func (l *Limiter) WaitN(ctx context.Context, n int64) error {
	if l == nil || n <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d := bytesToDuration(n, l.bps)

	l.mu.Lock()
	now := time.Now()
	start := l.next
	if start.Before(now) {
		start = now
	}
	end := start.Add(d)
	l.next = end
	l.mu.Unlock()

	wait := time.Until(start)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		l.releaseTail(start, end)
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// releaseTail drops this reservation when nobody has queued behind it.
// Leaving the tail in place when another caller already reserved past end
// avoids overlapping their slot. The unused gap is dead air, which is the
// safe direction for a production cap.
func (l *Limiter) releaseTail(start, end time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.next.Equal(end) {
		return
	}
	now := time.Now()
	if !now.After(start) {
		l.next = start
		return
	}
	if now.Before(end) {
		l.next = now
	}
}

// Wrap returns r, or a reader that charges r's bytes to lim.
// Each Read pulls at most ~100ms of budget (bounded to 1KiB..32KiB) and
// then waits, so the long-run average across all wrapped readers is the
// limiter's rate. The first chunk of each reader can leave the source
// before the wait; TCP backpressure holds the rest.
func Wrap(ctx context.Context, r io.Reader, lim *Limiter) io.Reader {
	if r == nil || lim == nil || lim.bps <= 0 {
		return r
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &limitedReader{ctx: ctx, r: r, lim: lim}
}

type limitedReader struct {
	ctx context.Context
	r   io.Reader
	lim *Limiter
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if q := r.lim.quantum(); len(p) > q {
		p = p[:q]
	}
	n, err := r.r.Read(p)
	if n > 0 {
		if werr := r.lim.WaitN(r.ctx, int64(n)); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// quantum is about 100ms of budget, clamped so a slow cap does not read
// one byte at a time and a fast cap does not pull a multi-megabyte burst.
func (l *Limiter) quantum() int {
	q := int(l.bps / 10)
	if q < 1024 {
		q = 1024
	}
	if q > 32*1024 {
		q = 32 * 1024
	}
	return q
}

func bytesToDuration(n, bps int64) time.Duration {
	if n <= 0 || bps <= 0 {
		return 0
	}
	ns := float64(n) * float64(time.Second) / float64(bps)
	if ns < 1 {
		return time.Nanosecond
	}
	if ns > float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}
