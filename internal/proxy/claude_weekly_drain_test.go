package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func weeklyDrainFixture(t *testing.T) *Server {
	t.Helper()
	now := time.Now()
	all := []accounts.Account{{ID: "later", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "later-token"}, {ID: "expiring", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "expiring-token"}, {ID: "third", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "third-token"}}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
	ref.usageWindows = make(map[string]usageWindowsEntry)
	scores := make([]selectacct.Score, 0, len(all))
	for _, account := range all {
		reset, weeklyUsed, shortUsed := now.Add(5*24*time.Hour), 20.0, 20.0
		if account.ID == "expiring" {
			reset, weeklyUsed, shortUsed = now.Add(3*time.Hour), 34, 48
		}
		windows := []accounts.UsageWindow{{Name: "5h", UsedPercent: shortUsed, LimitWindowSeconds: 5 * 3600, ResetAt: now.Add(2 * time.Hour)}, {Name: "7d", UsedPercent: weeklyUsed, LimitWindowSeconds: 7 * 24 * 3600, ResetAt: reset}}
		ref.usageWindows[account.ID+"\x00claude"] = usageWindowsEntry{windows: windows, at: now, orderAt: now, credentialKey: usageWindowsCredentialKey(account)}
		scores = append(scores, scoreFromUsageWindows(accounts.ProviderClaude, account.ID, windows))
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Accounts: all, AccountRef: ref, Sessions: store, admission: newAdmissionControl(), Scheduler: selectacct.NewScheduler(scores), MaxBodyBytes: 1 << 20, RefreshAccountFn: func(_ context.Context, a accounts.Account) (accounts.Account, error) { return a, nil }}
}

func weeklyDrainRequest(t *testing.T, s *Server, id string) (*http.Request, *upstreamAttempt) {
	t.Helper()
	if _, err := s.Sessions.Put("claude", id, "later", ""); err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest(http.MethodPost, "https://example.test/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer later-token")
	a := &upstreamAttempt{server: s, provider: accounts.ProviderClaude, agent: "claude", session: id, path: "/v1/messages", poolModel: "claude-sonnet", account: s.Accounts[0], getBody: r.GetBody, budget: newAttemptBudgetWithQuota(2, 2)}
	return r, a
}

func TestClaudeWeeklyDrainReservationsAndSessionClaim(t *testing.T) {
	s := weeklyDrainFixture(t)
	var attempts []*upstreamAttempt
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i))
			r, a := weeklyDrainRequest(t, s, id)
			layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: id, poolModel: "claude-sonnet", attempt: a}
			if _, _, ok := layer.beginWeeklyDrain(r); ok {
				mu.Lock()
				attempts = append(attempts, a)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(attempts) != defaultAdmissionLimit {
		t.Fatalf("reserved %d, want %d", len(attempts), defaultAdmissionLimit)
	}
	for _, a := range attempts {
		a.finishWeeklyDrain()
		a.finishWeeklyDrain()
	}
	if free, load := s.admission.freeLoad(admissionKeyFor(accounts.ProviderClaude, "expiring", "claude-sonnet")); !free || load != 0 {
		t.Fatalf("released: free=%v load=%d", free, load)
	}
	r, a := weeklyDrainRequest(t, s, "same")
	layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: "same", poolModel: "claude-sonnet", attempt: a}
	if _, _, ok := layer.beginWeeklyDrain(r); !ok {
		t.Fatal("first migration not claimed")
	}
	defer a.finishWeeklyDrain()
	r, b := weeklyDrainRequest(t, s, "same")
	layer.attempt = b
	if _, _, ok := layer.beginWeeklyDrain(r); ok {
		b.finishWeeklyDrain()
		t.Fatal("concurrent migration for same session")
	}
}

func TestClaudeWeeklyDrainRefreshCancellationReleases(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh", true: "cancel"}[cancel], func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			ctx, stop := context.WithCancel(r.Context())
			defer stop()
			r = r.WithContext(ctx)
			s.RefreshAccountFn = func(_ context.Context, a accounts.Account) (accounts.Account, error) {
				if cancel {
					stop()
					return a, nil
				}
				return a, errors.New("refresh failed")
			}
			layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: "session", poolModel: "claude-sonnet", attempt: a}
			if _, _, ok := layer.beginWeeklyDrain(r); ok {
				t.Fatal("failed refresh migrated")
			}
			if free, load := s.admission.freeLoad(admissionKeyFor(accounts.ProviderClaude, "expiring", "claude-sonnet")); !free || load != 0 {
				t.Fatalf("reservation leaked: %v %d", free, load)
			}
			release, ok := s.admission.claimWeeklyDrain(admissionSessionKey{agent: "claude", session: "session"})
			if !ok {
				t.Fatal("claim leaked")
			}
			release()
		})
	}
}

func TestClaudeWeeklyDrainHTTPResponseCommit(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		want                    string
	}{
		{"json", "application/json", `{"type":"message"}`, "expiring"},
		{"sse-success", "text/event-stream", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "expiring"},
		{"sse-failure", "text/event-stream", "event: error\ndata: {\"type\":\"error\"}\n\n", "later"},
		{"sse-truncated", "text/event-stream", "event: content_block_delta\ndata: {}\n\n", "later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer expiring-token" {
					t.Errorf("auth=%q", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", tc.contentType)
				io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			s.ClaudeUpstream, _ = url.Parse(upstream.URL)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			tr := (upstreamLayers{usageLimit: &usageLimitRetryTransport{maxAttempts: 3}}).build(http.DefaultTransport, a)
			response, err := tr.RoundTrip(r)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := s.Sessions.Get("claude", "session")
			if before.AccountID != "later" {
				t.Fatal("committed before reading body")
			}
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			assignment, _ := s.Sessions.Get("claude", "session")
			if assignment.AccountID != tc.want {
				t.Fatalf("assignment=%q want %q", assignment.AccountID, tc.want)
			}
		})
	}
}

func TestClaudeWeeklyDrainHandlerAndForcedSelector(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "forced"}[forced], func(t *testing.T) {
			s := weeklyDrainFixture(t)
			want := "expiring-token"
			if forced {
				want = "later-token"
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+want {
					t.Errorf("auth=%q", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"type":"message"}`)
			}))
			defer upstream.Close()
			s.ClaudeUpstream, _ = url.Parse(upstream.URL)
			s.Sessions.Put("claude", "session", "later", "")
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Subrouter-Agent", "claude")
			r.Header.Set("X-Subrouter-Session", "session")
			if forced {
				r.Header.Set("X-Subrouter-Account", "later")
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			assignment, _ := s.Sessions.Get("claude", "session")
			wantID := "expiring"
			if forced {
				wantID = "later"
			}
			if assignment.AccountID != wantID {
				t.Fatalf("assignment=%q", assignment.AccountID)
			}
		})
	}
}

func TestClaudeWeeklyDrainEvidenceAndGuards(t *testing.T) {
	for _, kind := range []string{"stale", "unknown", "credential", "blocked", "pinned", "preferred", "codex", "priority", "fable"} {
		t.Run(kind, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: "session", poolModel: "claude-sonnet", attempt: a}
			entry := s.AccountRef.usageWindows["expiring\x00claude"]
			switch kind {
			case "stale":
				entry.at = time.Now().Add(-3 * time.Minute)
			case "unknown":
				entry.windows[1].ResetAt = time.Time{}
			case "credential":
				entry.credentialKey = "another-credential"
			case "blocked":
				s.SchedulerRef = selectacct.NewSchedulerRef(s.Scheduler)
				s.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "expiring", "claude-sonnet", time.Now().Add(time.Hour))
			case "pinned":
				r.Header.Set("X-Subrouter-Account-ID", "later")
			case "preferred":
				layer.skipWeeklyDrain = true
			case "fable":
				layer.poolModel = "claude-fable"
			case "codex":
				layer.provider = accounts.ProviderCodex
			case "priority":
				s.AccountRef.mu.Lock()
				s.AccountRef.accounts = s.Accounts
				s.AccountRef.mu.Unlock()
				s.Accounts = nil
				if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
					t.Fatal(err)
				}
			}
			s.AccountRef.usageWindows["expiring\x00claude"] = entry
			if _, _, ok := layer.beginWeeklyDrain(r); ok {
				t.Fatal("excluded request migrated")
			}
		})
	}
}

func TestClaudeWeeklyDrainActualFallbackAndCAS(t *testing.T) {
	for _, kind := range []string{"quota-fallback", "cas", "truncated-json", "external-fallback", "outer-replay"} {
		t.Run(kind, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 && r.Header.Get("Authorization") != "Bearer expiring-token" {
					t.Errorf("first auth=%q", r.Header.Get("Authorization"))
				}
				switch kind {
				case "quota-fallback":
					if calls == 1 {
						// The original account becomes unavailable while the migrated request is
						// in flight, leaving the third account as the actual retry destination.
						s.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "later", "claude-sonnet", time.Now().Add(time.Hour))
						w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
						w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", time.Now().Add(time.Hour).Format(time.RFC3339))
						w.WriteHeader(429)
						io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
						return
					}
					if r.Header.Get("Authorization") != "Bearer third-token" {
						t.Errorf("fallback auth=%q", r.Header.Get("Authorization"))
					}
				case "cas":
					s.Sessions.Put("claude", "session", "third", "")
				case "truncated-json":
					w.Header().Set("Content-Length", "100")
				case "external-fallback":
					w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
					return
				case "outer-replay":
					if calls == 1 {
						w.WriteHeader(408)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			s.ClaudeUpstream, _ = url.Parse(upstream.URL)
			r.URL.Scheme, r.URL.Host = s.ClaudeUpstream.Scheme, s.ClaudeUpstream.Host
			if kind == "quota-fallback" {
				s.SchedulerRef = selectacct.NewSchedulerRef(s.Scheduler)
			}
			usage := &usageLimitRetryTransport{maxAttempts: 3}
			if kind == "external-fallback" {
				usage.maxAttempts = 1
				usage.fableFallback = func() (*http.Response, bool) {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, true
				}
			}
			layers := upstreamLayers{usageLimit: usage}
			if kind == "outer-replay" {
				layers.replayablePost = &replayablePostRetryTransport{maxAttempts: 2}
			}
			tr := layers.build(http.DefaultTransport, a)
			response, err := tr.RoundTrip(r)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if kind == "truncated-json" {
				if readErr == nil {
					t.Fatal("expected truncated response failure")
				}
			} else if readErr != nil {
				t.Fatal(readErr)
			}
			want := "later"
			if kind == "quota-fallback" || kind == "cas" {
				want = "third"
			}
			assignment, _ := s.Sessions.Get("claude", "session")
			if assignment.AccountID != want {
				t.Fatalf("assignment=%q want %q; calls=%d", assignment.AccountID, want, calls)
			}
		})
	}
}

func TestClaudeWeeklyDrainGenerationEndpoints(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/v1/messages", true},
		{http.MethodPost, "/messages", true},
		{http.MethodPost, "/v1/messages/count_tokens", false},
		{http.MethodPost, "/v1/messages/batches", false},
		{http.MethodPost, "/v1/complete", false},
		{http.MethodGet, "/v1/messages", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			r.Method, a.path = tc.method, tc.path
			layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: "session", poolModel: "claude-sonnet", attempt: a}
			if _, _, ok := layer.beginWeeklyDrain(r); ok != tc.want {
				t.Fatalf("migrated=%v want %v", ok, tc.want)
			}
		})
	}
}

func TestClaudeWeeklyDrainRefreshEvidenceFence(t *testing.T) {
	for _, kind := range []string{"credential", "blocked", "expired"} {
		t.Run(kind, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			s.SchedulerRef = selectacct.NewSchedulerRef(s.Scheduler)
			if kind == "expired" {
				entry := s.AccountRef.usageWindows["expiring\x00claude"]
				entry.windows[1].ResetAt = time.Now().Add(20 * time.Millisecond)
				s.AccountRef.usageWindows["expiring\x00claude"] = entry
			}
			s.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				switch kind {
				case "credential":
					account.Token = "replacement-credential"
				case "blocked":
					s.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, account.ID, "claude-sonnet", time.Now().Add(time.Hour))
				case "expired":
					time.Sleep(30 * time.Millisecond)
				}
				return account, nil
			}
			layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: "session", poolModel: "claude-sonnet", attempt: a}
			if _, _, ok := layer.beginWeeklyDrain(r); ok {
				t.Fatal("invalidated refresh evidence migrated")
			}
			if free, load := s.admission.freeLoad(admissionKeyFor(accounts.ProviderClaude, "expiring", "claude-sonnet")); !free || load != 0 {
				t.Fatalf("reservation leaked: %v %d", free, load)
			}
		})
	}
}

func TestClaudeWeeklyDrainNewPlacementNeedsNoCurrentEvidence(t *testing.T) {
	for _, freshPlacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "sticky", true: "new-placement"}[freshPlacement], func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			delete(s.AccountRef.usageWindows, "later\x00claude")
			layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: "session", poolModel: "claude-sonnet", attempt: a, weeklyDrainNewPlacement: freshPlacement}
			_, account, ok := layer.beginWeeklyDrain(r)
			if ok != freshPlacement {
				t.Fatalf("migrated=%v want %v", ok, freshPlacement)
			}
			if ok && (account.ID != "expiring" || a.weeklyDrainExpected != "later") {
				t.Fatalf("destination=%q expected-source=%q", account.ID, a.weeklyDrainExpected)
			}
		})
	}
}

func TestClaudeWeeklyDrainRetargetReleasesReservation(t *testing.T) {
	for _, via := range []string{"replay", "send"} {
		t.Run(via, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: "session", poolModel: "claude-sonnet", attempt: a}
			if _, _, ok := layer.beginWeeklyDrain(r); !ok {
				t.Fatal("no drain reservation")
			}
			if via == "replay" {
				next, err := a.replay(r, &s.Accounts[2])
				if err != nil {
					t.Fatal(err)
				}
				next.Body.Close()
			} else {
				response, err := a.send(proxyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if _, load := s.admission.freeLoad(admissionKeyFor(accounts.ProviderClaude, "expiring", "claude-sonnet")); load != 0 {
						t.Fatalf("old destination still reserved during new send: %d", load)
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
				}), r, s.Accounts[2])
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
			}
			if free, load := s.admission.freeLoad(admissionKeyFor(accounts.ProviderClaude, "expiring", "claude-sonnet")); !free || load != 0 {
				t.Fatalf("retarget leaked reservation: %v %d", free, load)
			}
		})
	}
}

func TestClaudeWeeklyDrainHandlerPreservesBindingDuringRefresh(t *testing.T) {
	s := weeklyDrainFixture(t)
	s.Sessions.Put("claude", "session", "later", "")
	refreshedInitial := false
	s.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if !refreshedInitial {
			refreshedInitial = true
			s.Sessions.Put("claude", "session", "third", "")
		}
		return account, nil
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	s.ClaudeUpstream, _ = url.Parse(upstream.URL)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Subrouter-Agent", "claude")
	r.Header.Set("X-Subrouter-Session", "session")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	assignment, _ := s.Sessions.Get("claude", "session")
	if assignment.AccountID != "third" {
		t.Fatalf("binding changed during refresh overwritten: %q", assignment.AccountID)
	}
}

func TestClaudeWeeklyDrainHandlerNewPlacementWithUnknownInitialQuota(t *testing.T) {
	s := weeklyDrainFixture(t)
	all := append([]accounts.Account(nil), s.Accounts...)
	s.Accounts = all[:1]
	delete(s.AccountRef.usageWindows, "later\x00claude")
	// The initial placement has no quota evidence. A newly available account
	// appears during refresh, before the transport makes its final decision.
	s.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		s.AccountRef.mu.Lock()
		s.AccountRef.accounts = all[1:]
		s.AccountRef.mu.Unlock()
		return account, nil
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer expiring-token" {
			t.Errorf("auth=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	s.ClaudeUpstream, _ = url.Parse(upstream.URL)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Subrouter-Agent", "claude")
	r.Header.Set("X-Subrouter-Session", "new-session")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d: %s", w.Code, w.Body.String())
	}
	assignment, _ := s.Sessions.Get("claude", "new-session")
	if assignment.AccountID != "expiring" {
		t.Fatalf("assignment=%q", assignment.AccountID)
	}
}

func TestClaudeWeeklyDrainFallbackKeepsWarmSource(t *testing.T) {
	for _, kind := range []string{"overload", "allowed429", "quota", "source-disabled", "source-hardmarked", "new-placement"} {
		t.Run(kind, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			// Keep the source routable but below the new-session floor, so the
			// existing retry selector deterministically serves the third account.
			scores := []selectacct.Score{
				{Provider: accounts.ProviderClaude, AccountID: "later", Headroom: .11, ShortHeadroom: .11},
				{Provider: accounts.ProviderClaude, AccountID: "expiring", Headroom: .52, ShortHeadroom: .52},
				{Provider: accounts.ProviderClaude, AccountID: "third", Headroom: .9, ShortHeadroom: .9},
			}
			s.Scheduler = selectacct.NewScheduler(scores)
			s.SchedulerRef = selectacct.NewSchedulerRef(s.Scheduler)
			s.UsageScoreTTL = time.Hour
			a.budget = newAttemptBudgetWithQuota(12, 12)
			s.ClaudeOverloadReroute = true
			if kind == "source-disabled" {
				s.AccountRef.mu.Lock()
				s.AccountRef.accounts = s.Accounts
				s.AccountRef.mu.Unlock()
				s.Accounts = nil
			}
			var targetCalls, thirdCalls int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer expiring-token" {
					targetCalls++
					switch kind {
					case "overload":
						w.WriteHeader(529)
						io.WriteString(w, `{"error":{"type":"overloaded_error"}}`)
						return
					case "source-disabled":
						if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: false}); err != nil {
							t.Error(err)
						}
					case "source-hardmarked":
						s.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "later", "claude-sonnet", time.Now().Add(time.Hour))
					}
					if kind == "allowed429" {
						w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
					} else {
						w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
						w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", time.Now().Add(time.Hour).Format(time.RFC3339))
					}
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer third-token" {
					t.Errorf("fallback auth=%q", r.Header.Get("Authorization"))
				}
				thirdCalls++
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			s.ClaudeUpstream, _ = url.Parse(upstream.URL)
			now := time.Now()
			tr := (upstreamLayers{usageLimit: &usageLimitRetryTransport{maxAttempts: 4, weeklyDrainNewPlacement: kind == "new-placement", now: func() time.Time { return now }, sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }}}).build(http.DefaultTransport, a)
			response, err := tr.RoundTrip(r)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || targetCalls == 0 || thirdCalls != 1 {
				t.Fatalf("status=%d target=%d third=%d", response.StatusCode, targetCalls, thirdCalls)
			}
			want := "later"
			if kind == "source-disabled" || kind == "source-hardmarked" || kind == "new-placement" {
				want = "third"
			}
			assignment, _ := s.Sessions.Get("claude", "session")
			if assignment.AccountID != want {
				t.Fatalf("assignment=%q want %q", assignment.AccountID, want)
			}
		})
	}
}

func TestClaudeWeeklyDrainRefreshRereadsLivePolicyAndQuota(t *testing.T) {
	for _, kind := range []string{"disabled", "priority", "quota", "identity"} {
		t.Run(kind, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			r, a := weeklyDrainRequest(t, s, "session")
			defer a.finishWeeklyDrain()
			s.AccountRef.mu.Lock()
			s.AccountRef.accounts = s.Accounts
			s.AccountRef.mu.Unlock()
			s.Accounts = nil
			s.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				switch kind {
				case "disabled":
					if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, account.ID, accounts.AccountPolicy{Enabled: false}); err != nil {
						t.Fatal(err)
					}
				case "priority":
					if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
						t.Fatal(err)
					}
				case "quota":
					s.AccountRef.usageWindowsMu.Lock()
					entry := s.AccountRef.usageWindows[account.ID+"\x00claude"]
					entry.windows = append([]accounts.UsageWindow(nil), entry.windows...)
					entry.windows[0].UsedPercent = 100
					entry.at = time.Now()
					s.AccountRef.usageWindows[account.ID+"\x00claude"] = entry
					s.AccountRef.usageWindowsMu.Unlock()
				case "identity":
					s.AccountRef.mu.Lock()
					updated := append([]accounts.Account(nil), s.AccountRef.accounts...)
					for i := range updated {
						if updated[i].ID == account.ID {
							updated[i].Token = "new-live-credential"
						}
					}
					s.AccountRef.accounts = updated
					s.AccountRef.mu.Unlock()
				}
				return account, nil
			}
			layer := usageLimitRetryTransport{server: s, provider: accounts.ProviderClaude, agent: "claude", session: "session", poolModel: "claude-sonnet", attempt: a}
			if _, _, ok := layer.beginWeeklyDrain(r); ok {
				t.Fatal("migrated using superseded policy or quota")
			}
			if free, load := s.admission.freeLoad(admissionKeyFor(accounts.ProviderClaude, "expiring", "claude-sonnet")); !free || load != 0 {
				t.Fatalf("reservation leaked: %v %d", free, load)
			}
			release, claimed := s.admission.claimWeeklyDrain(admissionSessionKey{agent: "claude", session: "session"})
			if !claimed {
				t.Fatal("migration claim leaked")
			}
			release()
		})
	}
}

func TestClaudeWeeklyDrainOriginalQuotaFailureCommitsActualFallback(t *testing.T) {
	s := weeklyDrainFixture(t)
	r, a := weeklyDrainRequest(t, s, "session")
	defer a.finishWeeklyDrain()
	s.AccountRef.mu.Lock()
	s.AccountRef.accounts = s.Accounts
	s.AccountRef.mu.Unlock()
	s.Accounts = nil
	if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "third", accounts.AccountPolicy{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	s.SchedulerRef = selectacct.NewSchedulerRef(s.Scheduler)
	s.UsageScoreTTL = time.Hour
	a.budget = newAttemptBudgetWithQuota(12, 12)
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		served = append(served, auth)
		if auth == "Bearer third-token" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
			return
		}
		if auth == "Bearer later-token" {
			if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "third", accounts.AccountPolicy{Enabled: true}); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", time.Now().Add(time.Hour).Format(time.RFC3339))
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	}))
	defer upstream.Close()
	s.ClaudeUpstream, _ = url.Parse(upstream.URL)
	tr := (upstreamLayers{usageLimit: &usageLimitRetryTransport{maxAttempts: 4}}).build(http.DefaultTransport, a)
	response, err := tr.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || len(served) != 3 || served[0] != "Bearer expiring-token" || served[1] != "Bearer later-token" || served[2] != "Bearer third-token" {
		t.Fatalf("status=%d actual attempts=%v", response.StatusCode, served)
	}
	assignment, _ := s.Sessions.Get("claude", "session")
	if assignment.AccountID != "third" {
		t.Fatalf("successful quota fallback not persisted: %q", assignment.AccountID)
	}
}

func TestClaudeWeeklyDrainHaikuIgnoresOtherModelQuota(t *testing.T) {
	const model = "claude-haiku-4-5"
	for _, kind := range []string{"ordinary", "stale-other-models", "shared-short-exhausted", "shared-weekly-exhausted", "haiku-exhausted"} {
		t.Run(kind, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			s.AccountRef.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
			target := s.Accounts[1]
			observed := time.Now()
			if kind == "stale-other-models" {
				observed = observed.Add(-5 * time.Minute)
			}
			// Neither another family's exhaustion nor its stale observation should
			// make Haiku lose access to the account's ordinary subscription quota.
			otherWindows := []accounts.UsageWindow{
				{Name: "opus-weekly", Feature: "claude-opus", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 3600, ResetAt: time.Now().Add(time.Hour)},
				{Name: "sonnet-weekly", Feature: "claude-sonnet", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 3600, ResetAt: time.Now().Add(time.Hour)},
			}
			if kind == "haiku-exhausted" {
				otherWindows = append(otherWindows, accounts.UsageWindow{Name: "haiku-weekly", Feature: model, UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 3600, ResetAt: time.Now().Add(time.Hour)})
			}
			s.AccountRef.claudeSupplemental = map[string]claudeSupplementalUsage{claudeSupplementalCacheKey(target): {at: observed, requestStarted: observed, windows: otherWindows}}
			if kind == "shared-short-exhausted" || kind == "shared-weekly-exhausted" {
				entry := s.AccountRef.usageWindows[target.ID+"\x00claude"]
				index := 0
				if kind == "shared-weekly-exhausted" {
					index = 1
				}
				entry.windows[index].UsedPercent = 100
				s.AccountRef.usageWindows[target.ID+"\x00claude"] = entry
			}
			wantID, wantToken := "expiring", "expiring-token"
			if kind == "shared-short-exhausted" || kind == "shared-weekly-exhausted" || kind == "haiku-exhausted" {
				wantID, wantToken = "later", "later-token"
			}
			var calls int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer "+wantToken {
					t.Errorf("auth=%q want account %q", r.Header.Get("Authorization"), wantID)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if !strings.Contains(string(body), model) {
					t.Errorf("Haiku model lost in request: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"type":"message"}`)
			}))
			defer upstream.Close()
			s.ClaudeUpstream, _ = url.Parse(upstream.URL)
			s.Sessions.Put("claude", "haiku-session", "later", "")
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Subrouter-Agent", "claude")
			r.Header.Set("X-Subrouter-Session", "haiku-session")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			assignment, _ := s.Sessions.Get("claude", "haiku-session")
			if assignment.AccountID != wantID {
				t.Fatalf("assignment=%q want %q", assignment.AccountID, wantID)
			}
		})
	}
}
