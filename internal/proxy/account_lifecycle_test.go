package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestAccountLifecyclePatchPreservesClearsAndNormalizesDates(t *testing.T) {
	handler, ref, _, _, _ := newAccountPolicyAdminServer(t)
	path := "/_subrouter/accounts/codex/shared"
	response := serveAccountPolicyAdmin(handler, http.MethodPatch, path, `{"ends_at":"2030-01-02T03:04:05+02:00","renews_at":"2029-11-01T00:00:00Z"}`, true)
	if response.Code != 200 {
		t.Fatalf("patch status=%d: %s", response.Code, response.Body.String())
	}
	original, err := ref.policyStore.Load(accounts.ProviderCodex, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if original.EndsAt.Format(time.RFC3339) != "2030-01-02T01:04:05Z" {
		t.Fatalf("date=%s", original.EndsAt)
	}
	response = serveAccountPolicyAdmin(handler, http.MethodPatch, path, `{"priority":12}`, true)
	if response.Code != 200 {
		t.Fatalf("partial status=%d", response.Code)
	}
	preserved, _ := ref.policyStore.Load(accounts.ProviderCodex, "shared")
	if !preserved.EndsAt.Equal(original.EndsAt) || !preserved.RenewsAt.Equal(original.RenewsAt) {
		t.Fatal("omitted dates changed")
	}
	for _, body := range []string{`{"ends_at":"tomorrow"}`, `{"ends_at":"0001-01-01T00:00:00Z"}`, `{"ends_at":123}`, `{"ends_at":""}`, `{"renews_at":{}}`, `{"ends_at":null,"ends_at":"2031-01-01T00:00:00Z"}`, `{"renews_at":null,"renews_at":null}`} {
		response = serveAccountPolicyAdmin(handler, http.MethodPatch, path, body, true)
		if response.Code != 400 {
			t.Fatalf("invalid patch %s status=%d", body, response.Code)
		}
		unchanged, _ := ref.policyStore.Load(accounts.ProviderCodex, "shared")
		if unchanged != preserved {
			t.Fatalf("invalid patch changed policy: %+v", unchanged)
		}
	}
	response = serveAccountPolicyAdmin(handler, http.MethodPatch, path, `{"ends_at":null,"renews_at":null}`, true)
	if response.Code != 200 {
		t.Fatalf("clear status=%d", response.Code)
	}
	cleared, _ := ref.policyStore.Load(accounts.ProviderCodex, "shared")
	if !cleared.EndsAt.IsZero() || !cleared.RenewsAt.IsZero() || cleared.Priority != 12 {
		t.Fatalf("cleared=%+v", cleared)
	}
}

func TestAccountLifecycleListAndUsageExposeEffectiveAvailability(t *testing.T) {
	account := policyRoutingAccount("ended")
	account.Provider = accounts.ProviderClaude
	s, policies := accountPolicyRoutingServer(t, account)
	end, renewal := time.Now().Add(-time.Hour).UTC().Truncate(time.Second), time.Now().Add(24*time.Hour).UTC().Truncate(time.Second)
	if err := policies.Update(account.Provider, account.ID, accounts.AccountPolicy{Enabled: true, EndsAt: end, RenewsAt: renewal}); err != nil {
		t.Fatal(err)
	}
	s.AccountRef.usageStatusCache = []AccountUsageStatus{{AccountStatus: AccountStatus{ID: account.ID, Provider: account.Provider, AuthValid: true}, Active: true, Windows: []accounts.UsageWindow{{Name: "5h"}, {Name: "7d"}}}}
	s.AccountRef.usageStatusAt = time.Now()
	for _, path := range []string{"/_subrouter/accounts", "/_subrouter/usage-status?snapshot=1"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var rows []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0]["enabled"] != true || rows[0]["available"] != false || rows[0]["ends_at"] != end.Format(time.RFC3339) || rows[0]["renews_at"] != renewal.Format(time.RFC3339) || rows[0]["active"] == true {
			t.Fatalf("%s: %+v", path, rows)
		}
	}
}

func lifecycleFixture(t *testing.T) *Server {
	t.Helper()
	s := weeklyDrainFixture(t)
	s.AccountRef.mu.Lock()
	s.AccountRef.accounts = s.Accounts
	s.AccountRef.mu.Unlock()
	s.Accounts = nil
	s.AccountRef.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
	return s
}

func lifecycleRequest(t *testing.T, s *Server, id string) (*http.Request, *upstreamAttempt) {
	t.Helper()
	if _, err := s.Sessions.Put("claude", id, "later", ""); err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest(http.MethodPost, "https://example.test/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer later-token")
	loaded, _, _ := s.AccountRef.CredentialSnapshot()
	source, ok := findAccount(loaded, "later")
	if !ok {
		t.Fatal("source absent")
	}
	a := &upstreamAttempt{server: s, provider: accounts.ProviderClaude, agent: "claude", session: id, path: "/v1/messages", poolModel: "claude-sonnet", account: source, getBody: r.GetBody, budget: newAttemptBudgetWithQuota(12, 12)}
	return r, a
}

func TestAccountLifecycleExpiredStickyAndPin(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "sticky-fallback", true: "pinned-error"}[pinned], func(t *testing.T) {
			s := lifecycleFixture(t)
			if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, EndsAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			s.Sessions.Put("claude", "session", "later", "")
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") == "Bearer later-token" {
					t.Error("ended account sent upstream")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			s.ClaudeUpstream, _ = url.Parse(upstream.URL)
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Subrouter-Agent", "claude")
			r.Header.Set("X-Subrouter-Session", "session")
			if pinned {
				r.Header.Set("X-Subrouter-Account-ID", "later")
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if pinned {
				if w.Code != 503 || calls != 0 || !strings.Contains(w.Body.String(), "ended at") {
					t.Fatalf("pin=%d calls=%d: %s", w.Code, calls, w.Body.String())
				}
			} else {
				if w.Code != 200 || calls != 1 {
					t.Fatalf("fallback=%d calls=%d: %s", w.Code, calls, w.Body.String())
				}
			}
			loaded, _, _ := s.AccountRef.CredentialSnapshot()
			if len(loaded) != 3 {
				t.Fatal("expired credential was removed")
			}
			if _, ok := s.Sessions.Get("claude", "session"); !ok {
				t.Fatal("session deleted")
			}
		})
	}
}

func TestAccountLifecyclePhysicalDeadlineAfterQueue(t *testing.T) {
	s := lifecycleFixture(t)
	s.admission.limit = 1
	account := accounts.Account{ID: "later", Provider: accounts.ProviderClaude}
	if err := s.AccountRef.policyStore.Update(account.Provider, account.ID, accounts.AccountPolicy{Enabled: true, EndsAt: time.Now().Add(80 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	key := admissionKeyFor(account.Provider, account.ID, "claude-sonnet")
	held, ok := s.admission.tryAcquire(key, admissionSessionKey{})
	if !ok {
		t.Fatal("could not occupy slot")
	}
	defer held.release()
	sent := false
	tr := inflightAttemptTransport{base: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) { sent = true; return nil, errors.New("must not send") }), attempt: &upstreamAttempt{server: s, account: account, poolModel: "claude-sonnet"}}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	_, err := tr.RoundTrip(r.WithContext(ctx))
	if !errors.Is(err, errAccountUnavailable) || sent || ctx.Err() != nil {
		t.Fatalf("err=%v sent=%v request=%v", err, sent, ctx.Err())
	}
	s.admission.mu.Lock()
	bucket := s.admission.buckets[key]
	active, queued := bucket.active, len(bucket.waiters)
	s.admission.mu.Unlock()
	if active != 1 || queued != 0 {
		t.Fatalf("active=%d queued=%d", active, queued)
	}
}

func TestAccountLifecycleRetryAfterDeadlineFallsBackAutomatically(t *testing.T) {
	s := lifecycleFixture(t)
	r, a := lifecycleRequest(t, s, "session")
	defer a.finishWeeklyDrain()
	s.SchedulerRef = selectacct.NewSchedulerRef(s.Scheduler)
	s.UsageScoreTTL = time.Hour
	a.budget = newAttemptBudgetWithQuota(12, 12)
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		auths = append(auths, auth)
		if len(auths) == 1 {
			if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, EndsAt: time.Now()}); err != nil {
				t.Error(err)
				return
			}
			w.WriteHeader(408)
			return
		}
		if auth == "Bearer later-token" {
			t.Error("retry sent after account end")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	s.ClaudeUpstream, _ = url.Parse(upstream.URL)
	r.URL.Scheme, r.URL.Host = s.ClaudeUpstream.Scheme, s.ClaudeUpstream.Host
	tr := (upstreamLayers{usageLimit: &usageLimitRetryTransport{maxAttempts: 4, skipWeeklyDrain: true}, replayablePost: &replayablePostRetryTransport{maxAttempts: 2}}).build(http.DefaultTransport, a)
	response, err := tr.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || len(auths) != 2 || auths[0] != "Bearer later-token" {
		t.Fatalf("status=%d auths=%v", response.StatusCode, auths)
	}
	assignment, _ := s.Sessions.Get("claude", "session")
	if assignment.AccountID == "later" {
		t.Fatal("fallback binding not persisted")
	}
}

func TestAccountLifecycleStartedStreamFinishesAfterDeadline(t *testing.T) {
	s := lifecycleFixture(t)
	r, a := lifecycleRequest(t, s, "session")
	defer a.finishWeeklyDrain()
	if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, EndsAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: content_block_delta\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, EndsAt: time.Now()}); err != nil {
			t.Error(err)
			return
		}
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	s.ClaudeUpstream, _ = url.Parse(upstream.URL)
	r.URL.Scheme, r.URL.Host = s.ClaudeUpstream.Scheme, s.ClaudeUpstream.Host
	tr := inflightAttemptTransport{base: http.DefaultTransport, attempt: a}
	response, err := tr.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !strings.Contains(string(body), "message_stop") {
		t.Fatalf("stream=%q err=%v", body, err)
	}
	if _, err := s.accountPolicyForAttempt(context.Background(), a.account, time.Now()); !errors.Is(err, errAccountUnavailable) {
		t.Fatalf("next attempt=%v", err)
	}
}

func TestAccountLifecycleAttemptMembership(t *testing.T) {
	account := accounts.Account{ID: "managed", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	for _, kind := range []string{"standalone", "static", "managed", "removed"} {
		t.Run(kind, func(t *testing.T) {
			s := &Server{}
			if kind != "standalone" {
				s.AccountRef = NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
			}
			if kind == "static" {
				s.Accounts = []accounts.Account{account}
				if err := s.AccountRef.policyStore.Update(account.Provider, account.ID, accounts.AccountPolicy{Enabled: false}); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "removed" {
				s.AccountRef.mu.Lock()
				s.AccountRef.accounts = nil
				s.AccountRef.mu.Unlock()
			}
			_, err := s.accountPolicyForAttempt(context.Background(), account, time.Now())
			if errors.Is(err, errAccountUnavailable) != (kind == "removed") {
				t.Fatalf("membership error=%v", err)
			}
		})
	}
}

func TestAccountLifecycleLeaseNeverOutlivesEnd(t *testing.T) {
	s := lifecycleFixture(t)
	deadline := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, EndsAt: deadline}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/_subrouter/leases", strings.NewReader(`{"provider":"claude","sessionId":"lease-session","forceAccountId":"later"}`))
	response := httptest.NewRecorder()
	newTenantCredentialLeaseStore().handleIssue(s, tenant.Tenant{ID: "team"}, response, request)
	if response.Code != 200 {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Lease struct {
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"lease"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Lease.ExpiresAt.Equal(deadline) {
		t.Fatalf("lease expires %s after account deadline %s", body.Lease.ExpiresAt, deadline)
	}
}

func TestAccountLifecycleQueuedRenewalReplacesObsoleteDeadline(t *testing.T) {
	for _, kind := range []string{"extended", "cleared", "cleared-then-cancelled"} {
		t.Run(kind, func(t *testing.T) {
			s := lifecycleFixture(t)
			s.admission.limit = 1
			key := admissionKeyFor(accounts.ProviderClaude, "later", "claude-sonnet")
			originalEnd := time.Now().Add(200 * time.Millisecond)
			if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, EndsAt: originalEnd}); err != nil {
				t.Fatal(err)
			}
			held, ok := s.admission.tryAcquire(key, admissionSessionKey{})
			if !ok {
				t.Fatal("could not occupy slot")
			}
			defer held.release()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			type result struct {
				lease *admissionLease
				err   error
			}
			results := make(chan result, 1)
			go func() {
				lease, err := s.acquireAccountAdmission(ctx, key, admissionSessionKey{agent: "claude", session: "renewed"})
				results <- result{lease, err}
			}()
			waitWebSocketAdmission(t, s.admission, 1, 1)
			renewedEnd := time.Time{}
			if kind == "extended" {
				renewedEnd = time.Now().Add(time.Hour)
			}
			if err := s.AccountRef.policyStore.Update(accounts.ProviderClaude, "later", accounts.AccountPolicy{Enabled: true, EndsAt: renewedEnd}); err != nil {
				t.Fatal(err)
			}
			<-time.After(time.Until(originalEnd) + 40*time.Millisecond)
			select {
			case got := <-results:
				got.lease.release()
				t.Fatalf("obsolete deadline ended renewed wait: %v", got.err)
			default:
			}
			waitWebSocketAdmission(t, s.admission, 1, 1)
			if kind == "cleared-then-cancelled" {
				cancel()
			} else {
				held.release()
			}
			select {
			case got := <-results:
				got.lease.release()
				if kind == "cleared-then-cancelled" {
					if !errors.Is(got.err, context.Canceled) {
						t.Fatalf("cancellation=%v", got.err)
					}
				} else if got.err != nil {
					t.Fatalf("renewed admission=%v", got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("renewed request did not finish")
			}
			held.release()
			if free, load := s.admission.freeLoad(key); !free || load != 0 {
				t.Fatalf("slot leaked: free=%v load=%d", free, load)
			}
		})
	}
}
