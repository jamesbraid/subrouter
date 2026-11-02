package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func demandSnapshot(t *testing.T, control *admissionControl, queued int) admissionKeySnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot := control.snapshot()
		if len(snapshot.Accounts) == 1 && snapshot.Accounts[0].Queued == queued {
			return snapshot.Accounts[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue did not reach %d: %+v", queued, snapshot)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAdmissionDemandMeasuresOfferedAttempts(t *testing.T) {
	control := testAdmissionControl(6, 64)
	key := admissionKeyFor(accounts.ProviderClaude, "test", "claudehaiku")
	var held []*admissionLease
	for range 6 {
		lease, ok := control.tryAcquire(key, admissionSessionKey{})
		if !ok {
			t.Fatal("expected initial admission")
		}
		held = append(held, lease)
		defer lease.release()
	}
	ready := make(chan *admissionLease, 4)
	for range 4 {
		go func() {
			lease, _ := control.acquire(context.Background(), key)
			ready <- lease
		}()
	}
	queued := demandSnapshot(t, control, 4)
	if queued.Active != 6 || queued.PeakDemand != 10 || queued.QueuedTotal != 4 || queued.QueueWaitCount != 0 {
		t.Fatalf("offered demand = %+v", queued)
	}
	for _, lease := range held {
		lease.release()
	}
	for range 4 {
		select {
		case lease := <-ready:
			lease.release()
		case <-time.After(3 * time.Second):
			t.Fatal("queued admission did not finish")
		}
	}
	finished := demandSnapshot(t, control, 0)
	if finished.Active != 0 || finished.PeakDemand != 10 || finished.QueuedTotal != 4 || finished.QueueWaitCount != 4 || finished.QueueWaitMSMax > finished.QueueWaitMSSum {
		t.Fatalf("completed demand = %+v", finished)
	}
}

func TestAdmissionDemandCountsCancelledAndRejectedWaits(t *testing.T) {
	control := testAdmissionControl(1, 1)
	key := admissionKeyFor(accounts.ProviderClaude, "test", "claudehaiku")
	held, _ := control.acquire(context.Background(), key)
	defer held.release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := control.acquire(ctx, key); done <- err }()
	demandSnapshot(t, control, 1)
	// Set a known queue residence so millisecond accounting is independent
	// of how quickly the machine schedules cancellation and pause handling.
	control.mu.Lock()
	control.buckets[key].waiters[0].enqueuedAt = time.Now().Add(-20 * time.Millisecond)
	control.mu.Unlock()
	if _, err := control.acquire(context.Background(), key); !errors.Is(err, errAdmissionQueueFull) {
		t.Fatalf("full queue error = %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	go func() { _, err := control.acquire(context.Background(), key); done <- err }()
	demandSnapshot(t, control, 1)
	control.mu.Lock()
	control.buckets[key].waiters[0].enqueuedAt = time.Now().Add(-30 * time.Millisecond)
	control.mu.Unlock()
	control.pause(key, time.Now().Add(time.Hour))
	if err := <-done; !errors.Is(err, errAdmissionPaused) {
		t.Fatalf("pause rejection = %v", err)
	}
	got := demandSnapshot(t, control, 0)
	if got.PeakDemand != 3 || got.QueuedTotal != 2 || got.QueueWaitCount != 2 || got.QueueWaitMSSum < 50 || got.QueueWaitMSMax < 30 {
		t.Fatalf("cancelled/rejected demand = %+v", got)
	}
}

func TestAdmissionDemandCancelGrantRaceCountsOnce(t *testing.T) {
	for range 30 {
		control := testAdmissionControl(1, 1)
		key := admissionKeyFor(accounts.ProviderClaude, "test", "claudehaiku")
		held, _ := control.acquire(context.Background(), key)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			lease, _ := control.acquire(ctx, key)
			lease.release()
			close(done)
		}()
		demandSnapshot(t, control, 1)
		var concurrent sync.WaitGroup
		concurrent.Add(2)
		go func() { defer concurrent.Done(); held.release() }()
		go func() { defer concurrent.Done(); cancel() }()
		concurrent.Wait()
		<-done
		got := demandSnapshot(t, control, 0)
		if got.Active != 0 || got.QueueWaitCount != 1 || got.QueuedTotal != 1 || got.PeakDemand != 2 {
			t.Fatalf("raced demand = %+v", got)
		}
	}
}
