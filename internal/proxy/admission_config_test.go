package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAdmissionConfiguredHaikuConcurrency(t *testing.T) {
	gate := newAdmissionControlWithLimits([]AdmissionLimit{{Provider: accounts.ProviderClaude, Model: "haiku", Limit: 16}})
	for _, tc := range []struct {
		provider accounts.Provider
		model    string
		want     int
	}{
		{accounts.ProviderClaude, "claude-haiku-4-5", 16},
		{accounts.ProviderClaude, "claude-opus", defaultAdmissionLimit},
		{accounts.ProviderClaude, "claude-sonnet", defaultAdmissionLimit},
		{accounts.ProviderCodex, "claude-haiku-4-5", defaultAdmissionLimit},
	} {
		t.Run(string(tc.provider)+"/"+tc.model, func(t *testing.T) {
			key := admissionKeyFor(tc.provider, "account", tc.model)
			var leases []*admissionLease
			defer func() {
				for _, lease := range leases {
					lease.release()
				}
			}()
			for i := 0; i < tc.want+1; i++ {
				lease, ok := gate.tryAcquire(key, admissionSessionKey{})
				if ok {
					leases = append(leases, lease)
				}
			}
			if len(leases) != tc.want {
				t.Fatalf("actual reserved slots=%d want %d", len(leases), tc.want)
			}
		})
	}
}

func TestAdmissionConfiguredConcurrentReservationBound(t *testing.T) {
	gate := newAdmissionControlWithLimits([]AdmissionLimit{{Provider: accounts.ProviderClaude, Model: "haiku", Limit: 16}})
	key := admissionKeyFor(accounts.ProviderClaude, "account", "claude-haiku-4-5")
	var wg sync.WaitGroup
	var mu sync.Mutex
	var leases []*admissionLease
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lease, ok := gate.tryAcquire(key, admissionSessionKey{}); ok {
				mu.Lock()
				leases = append(leases, lease)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()
	if len(leases) != 16 {
		t.Fatalf("concurrent reservations=%d want16", len(leases))
	}
}

func TestAdmissionConfiguredCancellationReleasesQueue(t *testing.T) {
	gate := newAdmissionControlWithLimits([]AdmissionLimit{{Provider: accounts.ProviderClaude, Model: "haiku", Limit: 16}})
	key := admissionKeyFor(accounts.ProviderClaude, "account", "claude-haiku-4-5")
	var leases []*admissionLease
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()
	for i := 0; i < 16; i++ {
		lease, ok := gate.tryAcquire(key, admissionSessionKey{})
		if !ok {
			t.Fatal("cap not available")
		}
		leases = append(leases, lease)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		lease, err := gate.acquireFor(ctx, key, admissionSessionKey{agent: "claude", session: "cancel"})
		lease.release()
		done <- err
	}()
	waitWebSocketAdmission(t, gate, 16, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	waitWebSocketAdmission(t, gate, 16, 0)
	for _, lease := range leases {
		lease.release()
	}
	if free, load := gate.freeLoad(key); !free || load != 0 {
		t.Fatalf("leak free=%v load=%d", free, load)
	}
}

func TestAdmissionConfigured429StillPausesAndRecovers(t *testing.T) {
	gate := newAdmissionControlWithLimits([]AdmissionLimit{{Provider: accounts.ProviderClaude, Model: "haiku", Limit: 16}})
	key := admissionKeyFor(accounts.ProviderClaude, "account", "claude-haiku-4-5")
	now := time.Now()
	gate.observe(key, 429, http.Header{"Anthropic-Ratelimit-Unified-Status": {"allowed"}, "Retry-After": {"1"}}, now)
	if lease, ok := gate.tryAcquire(key, admissionSessionKey{}); ok {
		lease.release()
		t.Fatal("429 did not pause configured account")
	}
	gate.mu.Lock()
	bucket := gate.buckets[key]
	pausedUntil := bucket.pausedTil
	for i, want := range []int{1, 2, 4, 8, 16} {
		if got, _ := gate.admissionLimitLocked(bucket, pausedUntil.Add(time.Duration(i)*admissionRecoveryStep)); got != want {
			t.Errorf("step%d effective=%d want%d", i, got, want)
		}
	}
	bucket.pausedTil = time.Now().Add(-2 * admissionRecoveryStep)
	gate.mu.Unlock()
	snapshot := gate.snapshot()
	if snapshot.Limit != defaultAdmissionLimit || len(snapshot.Limits) != 1 || snapshot.Limits[0].Limit != 16 || len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Limit != 16 || snapshot.Accounts[0].EffectiveLimit != 4 {
		t.Fatalf("configured/effective snapshot=%+v", snapshot)
	}
	var leases []*admissionLease
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()
	for i := 0; i < 5; i++ {
		if lease, ok := gate.tryAcquire(key, admissionSessionKey{}); ok {
			leases = append(leases, lease)
		}
	}
	if len(leases) != 4 {
		t.Fatalf("recovery admitted%d want4", len(leases))
	}
	gate.observe(key, 429, http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {time.Now().Add(time.Hour).Format(time.RFC3339)}}, time.Now())
	if gate.snapshot().Accounts[0].EffectiveLimit != 0 {
		t.Fatal("confirmed quota did not override configured cap")
	}
}

func TestParseAdmissionLimits(t *testing.T) {
	limits, err := ParseAdmissionLimits([]string{"claude/haiku=16", "claude/opus=4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 || limits[0].Model != "claude-haiku" || limits[0].Limit != 16 {
		t.Fatalf("limits=%+v", limits)
	}
	for _, values := range [][]string{{"claude/haiku=0"}, {"claude/haiku=65"}, {"claude/haiku=-1"}, {"claude/haiku=many"}, {"haiku=16"}, {"/haiku=16"}, {"claude/=16"}, {"claude/haiku=16", "claude/claude-haiku-4-5=8"}} {
		if _, err := ParseAdmissionLimits(values); err == nil {
			t.Fatalf("accepted invalid config%q", values)
		}
	}
}

func TestAdmissionConfiguredHandlerHaikuCap(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  int
	}{{"claude-haiku-4-5", 16}, {"claude-opus-4-8", 6}, {"claude-sonnet-5", 6}} {
		t.Run(tc.model, func(t *testing.T) {
			var active, peak atomic.Int32
			started := make(chan struct{}, 32)
			release := make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); current > old; old = peak.Load() {
					if peak.CompareAndSwap(old, current) {
						break
					}
				}
				started <- struct{}{}
				<-release
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			defer finish()
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
			ref.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
			account := accounts.Account{ID: "account", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
			upstreamURL, _ := url.Parse(upstream.URL)
			s := Server{Accounts: []accounts.Account{account}, AccountRef: ref, Sessions: mustPolicyRoutingSessionStore(t), MaxBodyBytes: 1 << 20, ClaudeUpstream: upstreamURL, AdmissionLimits: []AdmissionLimit{{Provider: accounts.ProviderClaude, Model: "haiku", Limit: 16}}, RefreshAccountFn: func(_ context.Context, a accounts.Account) (accounts.Account, error) { return a, nil }}
			handler := s.Handler()
			done := make(chan int, 24)
			for i := 0; i < 24; i++ {
				go func() {
					r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+tc.model+`","messages":[]}`))
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("X-Subrouter-Agent", "claude")
					r.Header.Set("X-Subrouter-Session", "same")
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					done <- w.Code
				}()
			}
			for i := 0; i < tc.want; i++ {
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					finish()
					t.Fatalf("started%d want%d", i, tc.want)
				}
			}
			select {
			case <-started:
				finish()
				t.Fatalf("exceeded configured cap%d", tc.want)
			case <-time.After(40 * time.Millisecond):
			}
			finish()
			for i := 0; i < 24; i++ {
				select {
				case status := <-done:
					if status != 200 {
						t.Errorf("status=%d", status)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("requests did not finish")
				}
			}
			if got := peak.Load(); got != int32(tc.want) {
				t.Fatalf("actual upstream peak=%d want%d", got, tc.want)
			}
		})
	}
}

func TestAdmissionHaikuAliasesShareOneConfiguredCap(t *testing.T) {
	gate := newAdmissionControlWithLimits([]AdmissionLimit{{Provider: accounts.ProviderClaude, Model: "haiku", Limit: 16}})
	aliases := []string{"claude-haiku-5-5", "claude-haiku-5-5-20261101", "claude-haiku-4-5", "haiku", "claude-haiku"}
	var leases []*admissionLease
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()
	for i := 0; i < 40; i++ {
		key := admissionKeyFor(accounts.ProviderClaude, "account", aliases[i%len(aliases)])
		if lease, ok := gate.tryAcquire(key, admissionSessionKey{}); ok {
			leases = append(leases, lease)
		}
	}
	if len(leases) != 16 {
		t.Fatalf("mixed-alias reservations=%d want one aggregate cap16", len(leases))
	}
	if len(gate.snapshot().Accounts) != 1 {
		t.Fatalf("Haiku aliases created%d physical buckets", len(gate.snapshot().Accounts))
	}
}

func TestAdmissionHaikuAliasesShare429PauseAndRecovery(t *testing.T) {
	gate := newAdmissionControlWithLimits([]AdmissionLimit{{Provider: accounts.ProviderClaude, Model: "haiku", Limit: 16}})
	aliases := []string{"claude-haiku-5-5", "claude-haiku-5-5-20261101", "claude-haiku-4-5", "haiku"}
	key := admissionKeyFor(accounts.ProviderClaude, "account", aliases[0])
	gate.observe(key, 429, http.Header{"Anthropic-Ratelimit-Unified-Status": {"allowed"}, "Retry-After": {"1"}}, time.Now())
	for _, model := range aliases {
		if lease, ok := gate.tryAcquire(admissionKeyFor(accounts.ProviderClaude, "account", model), admissionSessionKey{}); ok {
			lease.release()
			t.Errorf("alias%s bypassed real429 pause", model)
		}
	}
	gate.mu.Lock()
	gate.buckets[key].pausedTil = time.Now().Add(-2 * admissionRecoveryStep)
	gate.mu.Unlock()
	var leases []*admissionLease
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()
	for i := 0; i < 12; i++ {
		if lease, ok := gate.tryAcquire(admissionKeyFor(accounts.ProviderClaude, "account", aliases[i%len(aliases)]), admissionSessionKey{}); ok {
			leases = append(leases, lease)
		}
	}
	if len(leases) != 4 {
		t.Fatalf("mixed-alias recovery admitted%d want one aggregate cap4", len(leases))
	}
}

func TestAdmissionHaikuMatchingRejectsUnrelatedModels(t *testing.T) {
	gate := newAdmissionControlWithLimits([]AdmissionLimit{{Provider: accounts.ProviderClaude, Model: "haiku", Limit: 16}})
	for _, model := range []string{"third-party-haiku-preview", "haiku-third-party", "unrelated-claude-haiku-5-5"} {
		t.Run(model, func(t *testing.T) {
			key := admissionKeyFor(accounts.ProviderClaude, "account", model)
			var leases []*admissionLease
			defer func() {
				for _, lease := range leases {
					lease.release()
				}
			}()
			for i := 0; i < 17; i++ {
				if lease, ok := gate.tryAcquire(key, admissionSessionKey{}); ok {
					leases = append(leases, lease)
				}
			}
			if len(leases) != defaultAdmissionLimit {
				t.Fatalf("unrelated model got Haiku override: slots=%d want%d", len(leases), defaultAdmissionLimit)
			}
		})
	}
}
