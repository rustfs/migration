package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestUnlimitedIsFast(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 1<<20)
	start := time.Now()
	n, err := io.Copy(io.Discard, Wrap(context.Background(), bytes.NewReader(payload), nil))
	if err != nil || n != int64(len(payload)) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("nil limiter throttled a 1MiB copy: %s", time.Since(start))
	}
	if New(0) != nil || New(-5) != nil {
		t.Fatal("non-positive rates must be unlimited")
	}
}

func TestSingleReaderPace(t *testing.T) {
	const (
		n   = 100_000
		bps = 100_000
	)
	lim := New(bps)
	start := time.Now()
	got, err := io.Copy(io.Discard, Wrap(context.Background(), bytes.NewReader(bytes.Repeat([]byte("b"), n)), lim))
	elapsed := time.Since(start)
	if err != nil || got != n {
		t.Fatalf("n=%d err=%v", got, err)
	}
	// One quantum (~100ms, here 10KiB) is read before its cost delays the
	// next read, so wall time lands a little under n/bps.
	if elapsed < 700*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("elapsed %s, want about 1s", elapsed)
	}
}

func TestSharedAcrossReaders(t *testing.T) {
	const (
		readers = 4
		each    = 100_000
		bps     = 100_000
	)
	lim := New(bps)
	payload := bytes.Repeat([]byte("c"), each)
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(readers)
	errCh := make(chan error, readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			n, err := io.Copy(io.Discard, Wrap(context.Background(), bytes.NewReader(payload), lim))
			if err != nil || n != each {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	// 400000 bytes at 100000 B/s is about 4s when shared. Four private
	// limiters would finish near 1s. The floor sits between those two.
	if elapsed < 2*time.Second {
		t.Fatalf("elapsed %s, cap was not shared across readers", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("elapsed %s, limiter stalled", elapsed)
	}
}

func TestCancelReleasesReservation(t *testing.T) {
	lim := New(1000) // 1 KiB/s
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, Wrap(ctx, bytes.NewReader(bytes.Repeat([]byte("x"), 100_000)), lim))
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unblock the reader")
	}

	start := time.Now()
	n, err := io.Copy(io.Discard, Wrap(context.Background(), bytes.NewReader(bytes.Repeat([]byte("y"), 2000)), lim))
	if err != nil || n != 2000 {
		t.Fatalf("follow-up n=%d err=%v", n, err)
	}
	// 2KiB at 1KiB/s is about 2s. A stuck 100KiB reservation would be ~100s.
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancelled reservation was not released, took %s", time.Since(start))
	}
}
