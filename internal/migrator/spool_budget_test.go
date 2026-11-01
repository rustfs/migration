package migrator

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSpoolBudgetWaitReleaseAndCancellation(t *testing.T) {
	b := newSpoolBudget(10)
	release, err := b.acquire(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, err := b.acquire(ctx, 3); waiting <- err }()
	select {
	case err := <-waiting:
		t.Fatalf("reservation did not wait: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait cancellation: %v", err)
	}
	ready := make(chan func(), 1)
	go func() {
		done, err := b.acquire(context.Background(), 10)
		if err == nil {
			ready <- done
		}
	}()
	select {
	case <-ready:
		t.Fatal("reservation exceeded limit")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case done := <-ready:
		done()
	case <-time.After(time.Second):
		t.Fatal("release did not wake waiter")
	}
	if _, err := b.acquire(context.Background(), 11); err == nil {
		t.Fatal("oversized reservation succeeded")
	}
}

func TestSpoolBudgetRejectsOversizedObjectBeforeCopy(t *testing.T) {
	source := auditServer(t, map[string][]byte{"key": []byte("1234567890")})
	target := auditServer(t, nil)
	m, st := auditMigration(t, source.URL, target.URL)
	m.spoolBudget = newSpoolBudget(10) // data needs 10 bytes plus 1 byte for length checking
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("expected budget failure")
	}
	counts, err := st.Counts()
	if err != nil || counts.Failed != 1 {
		t.Fatalf("expected retryable failure: counts=%+v err=%v", counts, err)
	}
	if _, ok := target.Object("bucket", "key"); ok {
		t.Fatal("oversized object was copied")
	}
}
