package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func claudeResetFreshnessRef(t *testing.T, calls *atomic.Int32) (*AccountRef, accounts.Account) {
	t.Helper()
	transport := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.Path != "/api/oauth/usage" {
			t.Fatalf("unexpected request %s", req.URL)
		}
		body := `{"five_hour":{"utilization":0},"seven_day":{"utilization":3},"seven_day_oauth_apps":{"utilization":0},"cedar_ember":{"eligible":true,"grants":[{"id":"fable","label":"Fable","resets_left":1,"resets_total":1,"clears":["seven_day_overage_included"],"usable_now":true}]}}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	ref := cacheTestAccountRef(t, transport)
	loaded, err := ref.claudeStore.ListAccounts(context.Background())
	if err != nil || len(loaded) != 1 {
		t.Fatalf("accounts=%+v err=%v", loaded, err)
	}
	ref.accounts = loaded
	return ref, loaded[0]
}

func oldClaudeResetStatus() *accounts.LimitResetStatus {
	eligible := false
	return &accounts.LimitResetStatus{Known: true, Status: "live", Eligible: &eligible, Reason: "surface"}
}

func seedClaudeResetFreshness(ref *AccountRef, account accounts.Account) {
	now := time.Now()
	ref.usageWindows = map[string]usageWindowsEntry{account.ID + "\x00claude": {at: now, orderAt: now, credentialKey: usageWindowsCredentialKey(account), supplementalFresh: true, windows: healthyClaudeUpgradeWindows()}}
	ref.claudeLimitResets = map[string]claudeLimitResetEntry{claudeSupplementalCacheKey(account): {at: now.Add(-3 * time.Minute), status: oldClaudeResetStatus()}}
}

func assertFreshClaudeReset(t *testing.T, status *accounts.LimitResetStatus) {
	t.Helper()
	if status == nil || !status.Known || status.Eligible == nil || !*status.Eligible || len(status.Grants) != 1 || status.Grants[0].Remaining != 1 {
		t.Fatalf("reset status=%+v, want fresh Fable grant", status)
	}
}

func TestClaudeResetMetadataRefreshesDespitePassiveQuotaCache(t *testing.T) {
	var calls atomic.Int32
	ref, account := claudeResetFreshnessRef(t, &calls)
	seedClaudeResetFreshness(ref, account)
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), ref.client, account); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider reads=%d, want1 despite passive quota cache", calls.Load())
	}
	assertFreshClaudeReset(t, ref.claudeLimitResetStatus(account))
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), ref.client, account); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("fresh reset metadata triggered another provider read: %d", calls.Load())
	}
}

func TestClaudeResetMetadataRefreshesDespiteFreshStatusSnapshot(t *testing.T) {
	var calls atomic.Int32
	ref, account := claudeResetFreshnessRef(t, &calls)
	seedClaudeResetFreshness(ref, account)
	ref.usageStatusAt = time.Now()
	ref.usageStatusCache = []AccountUsageStatus{{AccountStatus: AccountStatus{ID: account.ID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}, LimitResets: oldClaudeResetStatus(), UsageFetchedAt: time.Now()}}
	rows := ref.UsageStatuses(context.Background())
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	assertFreshClaudeReset(t, rows[0].LimitResets)
	if calls.Load() != 1 {
		t.Fatalf("provider reads=%d, want1", calls.Load())
	}
}

func TestClaudeResetMetadataPromotesWithoutNewQuotaTimestamp(t *testing.T) {
	var calls atomic.Int32
	ref, account := claudeResetFreshnessRef(t, &calls)
	seedClaudeResetFreshness(ref, account)
	current := time.Now()
	ref.usageStatusCache = []AccountUsageStatus{{AccountStatus: AccountStatus{ID: account.ID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}, UsageFetchedAt: current, LimitResets: oldClaudeResetStatus()}}
	eligible := true
	ref.rememberClaudeLimitResets(account, &accounts.LimitResetStatus{Known: true, Status: "live", Eligible: &eligible, Grants: []accounts.LimitResetGrant{{ID: "fable", Remaining: 1, UsableNow: true}}})
	ref.RefreshUsageStatusSnapshotFromWindows()
	rows, _ := ref.UsageStatusSnapshot()
	assertFreshClaudeReset(t, rows[0].LimitResets)
	if !rows[0].UsageFetchedAt.Equal(current) || calls.Load() != 0 {
		t.Fatal("reset publication changed quota timestamp or called provider")
	}
}

func TestClaudeResetMetadataHonorsTelemetryHoldAndDoesNotClaimLive(t *testing.T) {
	var calls atomic.Int32
	ref, account := claudeResetFreshnessRef(t, &calls)
	seedClaudeResetFreshness(ref, account)
	observed := time.Now().Add(-6 * time.Minute)
	key := account.ID + "\x00claude"
	entry := ref.usageWindows[key]
	entry.at = observed
	entry.orderAt = observed
	ref.usageWindows[key] = entry
	retryAt := time.Now().Add(3 * time.Minute)
	ref.usageWindowsFailures = map[string]usageWindowsFailure{key: {err: errPersistedUsageThrottle, retryAt: retryAt, credentialKey: usageWindowsCredentialKey(account)}}
	old := AccountUsageStatus{AccountStatus: AccountStatus{ID: account.ID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}, UsageFetchedAt: observed, Windows: healthyClaudeUpgradeWindows(), LimitResets: oldClaudeResetStatus()}
	ref.lastGoodUsage = map[string]usageStatusSnapshot{key: {status: old, at: observed}}
	response := httptest.NewRecorder()
	Server{AccountRef: ref}.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/_subrouter/usage-status?refresh=1", nil))
	var rows []AccountUsageStatus
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].UsageThrottled {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0].LimitResets == nil || rows[0].LimitResets.Known || rows[0].LimitResets.Status != "throttled" {
		t.Fatalf("old refusal advertised as live: %s", response.Body.String())
	}
	if calls.Load() != 0 || !ref.usageWindowsFailures[key].retryAt.Equal(retryAt) {
		t.Fatal("status refresh bypassed provider Retry-After")
	}
	ref.usageWindowsMu.Lock()
	failure := ref.usageWindowsFailures[key]
	failure.retryAt = time.Now().Add(-time.Second)
	ref.usageWindowsFailures[key] = failure
	ref.usageWindowsMu.Unlock()
	rows = ref.UsageStatuses(context.Background())
	assertFreshClaudeReset(t, rows[0].LimitResets)
	if calls.Load() != 1 {
		t.Fatalf("reads after hold=%d, want1", calls.Load())
	}
}

func TestClaudeResetMetadataRefreshRemainsSingleflight(t *testing.T) {
	var calls atomic.Int32
	ref, account := claudeResetFreshnessRef(t, &calls)
	seedClaudeResetFreshness(ref, account)
	const readers = 16
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			if _, _, err := ref.FetchUsageWindowsCached(context.Background(), ref.client, account); err != nil {
				t.Error(err)
			}
		}()
	}
	close(ready)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d concurrent readers made%d provider requests, want1", readers, calls.Load())
	}
	assertFreshClaudeReset(t, ref.claudeLimitResetStatus(account))
}

func TestClaudeResetMetadataFailureDoesNotPollOnEveryStatusRead(t *testing.T) {
	var calls atomic.Int32
	ref, account := claudeResetFreshnessRef(t, &calls)
	seedClaudeResetFreshness(ref, account)
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		code := http.StatusOK
		h := make(http.Header)
		if req.URL.Path == "/api/oauth/usage" {
			calls.Add(1)
			code = http.StatusInternalServerError
		} else {
			h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.03")
			h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		}
		return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: h, Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
	})}
	for i := 0; i < 4; i++ {
		if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("failed reset metadata was retried%d times in oneTTL, want1", calls.Load())
	}
	status := ref.claudeLimitResetStatus(account)
	if status.Known || status.Status != "unavailable" {
		t.Fatalf("failed reset metadata=%+v", status)
	}
}

func TestClaudeResetMetadataDoesNotPollInferenceOnlyCredentials(t *testing.T) {
	var calls atomic.Int32
	ref, account := claudeResetFreshnessRef(t, &calls)
	seedClaudeResetFreshness(ref, account)
	ref.rememberClaudeUsageScopeDenied(account)
	for i := 0; i < 4; i++ {
		if _, _, err := ref.FetchUsageWindowsCached(context.Background(), ref.client, account); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("inference-only credential triggered%d reset reads", calls.Load())
	}
}

func TestClaudeResetMetadataIgnoresOlderObservation(t *testing.T) {
	var calls atomic.Int32
	ref, account := claudeResetFreshnessRef(t, &calls)
	later := time.Now()
	eligible := true
	ref.rememberClaudeLimitResetsAfter(account, &accounts.LimitResetStatus{Known: true, Status: "live", Eligible: &eligible, Grants: []accounts.LimitResetGrant{{ID: "fable", Remaining: 1}}}, later)
	ref.rememberClaudeLimitResetsAfter(account, oldClaudeResetStatus(), later.Add(-time.Second))
	assertFreshClaudeReset(t, ref.claudeLimitResetStatus(account))
}
