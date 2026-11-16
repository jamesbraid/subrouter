package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func admissionWorkerForTest(t *testing.T, limit int) *admissionControl {
	t.Helper()
	control := newAdmissionControl()
	control.limit = limit
	t.Cleanup(func() {
		if control.shared != nil && control.shared.active != nil {
			_ = control.shared.active.Close()
		}
	})
	return control
}

func TestAdmissionLimitAcrossWorkerGenerations(t *testing.T) {
	t.Setenv("SUBROUTER_ADMISSION_DIR", t.TempDir())
	oldWorker, newWorker := admissionWorkerForTest(t, 1), admissionWorkerForTest(t, 1)
	key := admissionKeyFor(accounts.ProviderCodex, "shared-account", "")
	old, err := oldWorker.acquire(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer old.release()
	if extra, admitted := newWorker.tryAcquire(key, admissionSessionKey{}); admitted {
		extra.release()
		t.Fatal("overlapping workers exceeded the account's single admission slot")
	}
	if free, load := newWorker.freeLoad(key); free || load != 1 {
		t.Fatalf("routing ignored the retiring worker's load: free=%v load=%d", free, load)
	}
	snapshot := newWorker.snapshot()
	if !snapshot.Shared || len(snapshot.Accounts) != 1 || snapshot.Accounts[0].SharedActive == nil || *snapshot.Accounts[0].SharedActive != 1 {
		t.Fatalf("shared occupancy not visible: %+v", snapshot)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	waiting := make(chan *admissionLease, 1)
	errors := make(chan error, 1)
	go func() {
		lease, err := newWorker.acquire(ctx, key)
		if err != nil {
			errors <- err
			return
		}
		waiting <- lease
	}()
	select {
	case lease := <-waiting:
		lease.release()
		t.Fatal("replacement worker bypassed the occupied shared slot")
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(30 * time.Millisecond):
	}
	old.release()
	select {
	case lease := <-waiting:
		lease.release()
	case err := <-errors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("replacement worker did not wake after the retired worker released its slot")
	}
}

func TestSharedAdmissionCancellationDoesNotLoseCapacity(t *testing.T) {
	t.Setenv(sharedAdmissionEnv, t.TempDir())
	first, second := admissionWorkerForTest(t, 1), admissionWorkerForTest(t, 1)
	key := admissionKeyFor(accounts.ProviderCodex, "account", "")
	held, err := first.acquire(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if lease, err := second.acquire(ctx, key); err == nil {
		lease.release()
		t.Fatal("request bypassed the held slot")
	}
	held.release()
	next, err := second.acquire(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	next.release()
	if snapshot := second.snapshot(); len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Active != 0 || snapshot.Accounts[0].Queued != 0 {
		t.Fatalf("cancelled waiter or completed attempt leaked: %+v", snapshot.Accounts)
	}
}

func TestSharedAdmissionRefusesDifferentLiveLimits(t *testing.T) {
	t.Setenv(sharedAdmissionEnv, t.TempDir())
	first := admissionWorkerForTest(t, 1)
	key := admissionKeyFor(accounts.ProviderCodex, "account", "")
	held, err := first.acquire(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	held.release()
	// An idle generation still owns its policy until it exits. A candidate
	// cannot reinterpret the slot files under different limits during overlap.
	candidate := admissionWorkerForTest(t, defaultAdmissionLimit)
	if err := candidate.shared.prepare(candidate.limit, candidate.modelLimits); err == nil || !strings.Contains(err.Error(), "admission limits differ") {
		t.Fatalf("different running-generation policy was accepted: %v", err)
	}
	response := httptest.NewRecorder()
	Server{admission: candidate}.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("incompatible candidate became ready: %d %s", response.Code, response.Body.String())
	}
	if err := first.shared.active.Close(); err != nil {
		t.Fatal(err)
	}
	// With no older generation alive, a new policy may be installed normally.
	response = httptest.NewRecorder()
	Server{admission: admissionWorkerForTest(t, defaultAdmissionLimit)}.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/ready", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("new policy rejected after prior generation exited: %d %s", response.Code, response.Body.String())
	}
}

func TestSharedAdmissionFailsClosedOnInvalidDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dir, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sharedAdmissionEnv, dir)
	control := admissionWorkerForTest(t, defaultAdmissionLimit)
	if lease, err := control.acquire(t.Context(), admissionKeyFor(accounts.ProviderCodex, "account", "")); err == nil {
		lease.release()
		t.Fatal("invalid shared store admitted an attempt")
	}
	response := httptest.NewRecorder()
	Server{admission: admissionWorkerForTest(t, defaultAdmissionLimit)}.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("candidate with invalid store became ready: %d", response.Code)
	}
}

func TestSharedAdmissionReleasesOnWorkerExit(t *testing.T) {
	key := admissionKeyFor(accounts.ProviderCodex, "worker-account", "")
	if os.Getenv("SUBROUTER_SHARED_ADMISSION_HELPER") == "1" {
		control := newAdmissionControl()
		control.limit = 1
		lease, err := control.acquire(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.release()
		fmt.Println("shared-slot-ready")
		for {
			time.Sleep(time.Minute)
		}
	}
	t.Setenv(sharedAdmissionEnv, t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSharedAdmissionReleasesOnWorkerExit$")
	child.Env = append(os.Environ(), "SUBROUTER_SHARED_ADMISSION_HELPER=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "shared-slot-ready" {
		t.Fatal("worker did not acquire its slot")
	}
	other := admissionWorkerForTest(t, 1)
	if lease, admitted := other.tryAcquire(key, admissionSessionKey{}); admitted {
		lease.release()
		t.Fatal("live worker's permit was reused")
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	lease, err := other.acquire(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
}

func TestSharedAdmissionDoesNotStarveAnotherGeneration(t *testing.T) {
	t.Setenv(sharedAdmissionEnv, t.TempDir())
	busy, olderSession := admissionWorkerForTest(t, 1), admissionWorkerForTest(t, 1)
	key := admissionKeyFor(accounts.ProviderCodex, "busy-account", "")
	held, err := busy.acquire(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type result struct {
		lease *admissionLease
		err   error
	}
	old, next := make(chan result, 1), make(chan result, 1)
	start := func(control *admissionControl, ready chan result) {
		go func() {
			lease, err := control.acquire(ctx, key)
			ready <- result{lease, err}
		}()
	}
	waitQueued := func(control *admissionControl) {
		t.Helper()
		for ctx.Err() == nil {
			control.mu.Lock()
			bucket := control.buckets[key]
			queued := bucket != nil && len(bucket.waiters) == 1
			control.mu.Unlock()
			if queued {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("request did not enter the admission queue")
	}
	start(olderSession, old)
	waitQueued(olderSession)
	start(busy, next)
	waitQueued(busy)
	// Delay the older generation's poll. Releasing a slot must not let the
	// busy generation's synchronous local wake take it back ahead of that waiter.
	olderSession.mu.Lock()
	held.release()
	busy.mu.Lock()
	reclaimed := busy.buckets[key].active
	busy.mu.Unlock()
	olderSession.mu.Unlock()
	if reclaimed != 0 {
		cancel()
		for _, ready := range []chan result{old, next} {
			if value := <-ready; value.lease != nil {
				value.lease.release()
			}
		}
		t.Fatal("busy generation reclaimed the slot ahead of an older waiting generation")
	}
	value := <-old
	if value.err != nil {
		t.Fatal(value.err)
	}
	value.lease.release()
	value = <-next
	if value.err != nil {
		t.Fatal(value.err)
	}
	value.lease.release()
}

func TestSharedAdmissionCancellationWhileMetadataIsBusy(t *testing.T) {
	t.Setenv(sharedAdmissionEnv, t.TempDir())
	first, waiting := admissionWorkerForTest(t, 1), admissionWorkerForTest(t, 1)
	key := admissionKeyFor(accounts.ProviderCodex, "metadata-account", "")
	held, err := first.acquire(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		lease, err := waiting.acquire(ctx, key)
		if lease != nil {
			lease.release()
		}
		finished <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		waiting.mu.Lock()
		bucket := waiting.buckets[key]
		queued := bucket != nil && len(bucket.waiters) == 1 && bucket.waiters[0].ticket != nil
		waiting.mu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not register its shared waiter")
		}
		time.Sleep(time.Millisecond)
	}
	guard, available, err := first.shared.queueGuard(sharedAdmissionDigest(key))
	if err != nil || !available {
		t.Fatalf("hold metadata lock: available=%v error=%v", available, err)
	}
	defer guard.Close()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled request returned %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancellation waited for another generation's metadata lock")
	}
	if lease, admitted := waiting.tryAcquire(admissionKeyFor(accounts.ProviderCodex, "other-account", ""), admissionSessionKey{}); !admitted {
		t.Fatal("busy metadata blocked an unrelated account")
	} else {
		lease.release()
	}
	_ = guard.Close()
	held.release()
	if free, load := waiting.freeLoad(key); !free || load != 0 {
		t.Fatalf("cancelled shared waiter retained priority: free=%v load=%d", free, load)
	}
}

func TestSharedAdmissionRoutingHonorsForeignQueueHead(t *testing.T) {
	t.Setenv(sharedAdmissionEnv, t.TempDir())
	first, second := admissionWorkerForTest(t, 1), admissionWorkerForTest(t, 1)
	key := admissionKeyFor(accounts.ProviderCodex, "queued-account", "")
	if err := first.shared.prepare(first.limit, first.modelLimits); err != nil {
		t.Fatal(err)
	}
	digest := sharedAdmissionDigest(key)
	guard, available, err := first.shared.queueGuard(digest)
	if err != nil || !available {
		t.Fatalf("hold queue metadata: %v", err)
	}
	waiter, err := first.shared.enqueueLocked(digest)
	_ = guard.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.release()
	if free, load := second.freeLoad(key); free || load < 1 {
		t.Fatalf("routing ignored a foreign queue head: free=%v load=%d", free, load)
	}
	if lease, admitted := second.tryAcquire(key, admissionSessionKey{}); admitted {
		lease.release()
		t.Fatal("new attempt bypassed the foreign queue head")
	}
	waiter.release()
	if free, load := second.freeLoad(key); !free || load != 0 {
		t.Fatalf("cancelled foreign head did not release capacity: free=%v load=%d", free, load)
	}
}

func TestSharedAdmissionCancelledRejectedWaiterReleasesQueue(t *testing.T) {
	t.Setenv(sharedAdmissionEnv, t.TempDir())
	control, other := admissionWorkerForTest(t, 1), admissionWorkerForTest(t, 1)
	key := admissionKeyFor(accounts.ProviderCodex, "rejected-account", "")
	if err := control.shared.prepare(control.limit, control.modelLimits); err != nil {
		t.Fatal(err)
	}
	digest := sharedAdmissionDigest(key)
	guard, available, err := control.shared.queueGuard(digest)
	if err != nil || !available {
		t.Fatalf("hold queue metadata: %v", err)
	}
	ticket, err := control.shared.enqueueLocked(digest)
	_ = guard.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer ticket.release()
	// A wake registered the global ticket, then a quota pause removed the
	// local waiter. Cancellation may win over the already-closed ready channel.
	control.cancelOrReleaseWaiter(key, &admissionWaiter{rejected: true, ticket: ticket})
	if free, load := other.freeLoad(key); !free || load != 0 {
		t.Fatalf("rejected cancellation left an owned queue head: free=%v load=%d", free, load)
	}
}
