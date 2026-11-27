package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestClaudeInferenceOnlyQuotaRecoveryWithoutFiveHourWindow(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		fableHasWeekly          bool
		primaryStatus           int
		primaryAllowed          string
		modelHold, throttleHold bool
		wantPaused              bool
	}{
		{name: "credit-gated Fable falls back to accepted primary", primaryStatus: 200, primaryAllowed: "allowed"},
		{name: "Fable weekly headers still require primary acceptance", fableHasWeekly: true, primaryStatus: 200, primaryAllowed: "allowed"},
		{name: "warning is accepted", primaryStatus: 200, primaryAllowed: "allowed_warning"},
		{name: "successful response without allowance is insufficient", primaryStatus: 200, wantPaused: true},
		{name: "rate limit is not acceptance", primaryStatus: 429, primaryAllowed: "allowed", wantPaused: true},
		{name: "rejected quota is not acceptance", primaryStatus: 200, primaryAllowed: "rejected", wantPaused: true},
		{name: "preserve independent model quota", primaryStatus: 200, primaryAllowed: "allowed", modelHold: true, wantPaused: true},
		{name: "preserve independent throttle", primaryStatus: 200, primaryAllowed: "allowed", throttleHold: true, wantPaused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := accounts.Account{ID: "inference-only", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
			ref.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
			gate := newAdmissionControl()
			key := admissionKeyFor(accounts.ProviderClaude, account.ID, agentclaude.SonnetFeature)
			gate.pauseWithQuotaScope(key, time.Now().Add(5*24*time.Hour), false, false)
			if tc.modelHold {
				gate.pauseWithQuotaScope(key, time.Now().Add(time.Hour), false, true)
			}
			if tc.throttleHold {
				gate.pauseWithQuotaScope(key, time.Now().Add(time.Hour), true, false)
			}
			primaryCalls := 0
			client := &http.Client{Transport: proxyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				code, body := http.StatusForbidden, `{"error":{"type":"permission_error","details":{"error_code":"oauth_scope_insufficient"}}}`
				h := make(http.Header)
				if r.URL.Path == "/v1/messages" {
					var payload struct {
						Model string `json:"model"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload.Model == agentclaude.FableModel {
						code, body = http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","details":{"error_code":"credits_required"}}}`
						if tc.fableHasWeekly {
							code = 200
							h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.03")
							h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
						}
					} else {
						primaryCalls++
						code, body = tc.primaryStatus, `{"type":"message"}`
						h.Set("Anthropic-Ratelimit-Unified-Status", tc.primaryAllowed)
						h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.03")
						h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
						h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(time.Now().Add(5*24*time.Hour).Unix(), 10))
						h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
					}
				}
				return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			server := Server{AccountRef: ref, admission: gate}
			windows, _, err := server.fetchAccountUsageWindows(context.Background(), client, account)
			if err != nil {
				t.Fatal(err)
			}
			if primaryCalls != 1 {
				t.Fatalf("ordinary-model probes=%d, want1", primaryCalls)
			}
			if usageWindowNamed(windows, "5h") {
				t.Fatalf("fabricated five-hour quota: %+v", windows)
			}
			if got := gate.paused(key, time.Now()); got != tc.wantPaused {
				t.Fatalf("paused=%v, want%v after explicit primary allowance", got, tc.wantPaused)
			}
		})
	}
}

func TestClaudePrimaryAllowancePreservesNewerRejectionsAndCredentials(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		old, expired, replaced bool
	}{
		{name: "older request", old: true},
		{name: "expired allowance", expired: true},
		{name: "superseded credential", replaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := accounts.Account{ID: "inference-only", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "current"}
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
			ref.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
			gate := newAdmissionControl()
			key := admissionKeyFor(accounts.ProviderClaude, account.ID, agentclaude.SonnetFeature)
			started := time.Now()
			gate.pauseWithQuotaScope(key, time.Now().Add(time.Hour), false, false)
			if !tc.old {
				started = time.Now()
			}
			if tc.expired {
				started = time.Now().Add(-2 * usageWindowsTTL)
			}
			observed := time.Now()
			ref.usageWindows = map[string]usageWindowsEntry{account.ID + "\x00claude": {at: observed, orderAt: observed, credentialKey: usageWindowsCredentialKey(account), windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 3, LimitWindowSeconds: 604800}}}}
			if tc.replaced {
				account.Token = "previous"
			}
			ref.noteClaudePrimaryAllowance(account, started)
			Server{AccountRef: ref, admission: gate}.reconcileClaudeAdmissionQuota()
			if !gate.paused(key, time.Now()) {
				t.Fatal("stale or superseded acceptance cleared pause")
			}
		})
	}
}

func TestClaudeOrdinaryResponseRecoversWeeklyOnlyAdmission(t *testing.T) {
	for _, model := range []string{"claude-haiku-5-5", agentclaude.FableModel} {
		t.Run(model, func(t *testing.T) {
			account := accounts.Account{ID: "inference-only", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
			ref.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
			gate := newAdmissionControl()
			key := admissionKeyFor(accounts.ProviderClaude, account.ID, agentclaude.SonnetFeature)
			gate.pauseWithQuotaScope(key, time.Now().Add(time.Hour), false, false)
			observed := time.Now()
			ref.usageWindows = map[string]usageWindowsEntry{account.ID + "\x00claude": {at: observed, orderAt: observed, credentialKey: usageWindowsCredentialKey(account), windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 3, LimitWindowSeconds: 604800}}}}
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			upstream, _ := url.Parse("https://claude.example")
			server := Server{AccountRef: ref, admission: gate, Sessions: store, Scheduler: selectacct.NewScheduler([]selectacct.Score{{Provider: accounts.ProviderClaude, AccountID: account.ID, Headroom: 0.97, ShortHeadroom: 0.97}}), ClaudeUpstream: upstream, MaxBodyBytes: 1 << 20, RefreshAccountFn: func(_ context.Context, a accounts.Account) (accounts.Account, error) { return a, nil }, Transport: proxyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
				h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
				h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.03")
				h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"message"}`)), Request: r}, nil
			})}
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Subrouter-Agent", "claude")
			server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("response=%d: %s", response.Code, response.Body.String())
			}
			wantPaused := model == agentclaude.FableModel
			if got := gate.paused(key, time.Now()); got != wantPaused {
				t.Fatalf("Sonnet paused=%v, want%v after %s acceptance", got, wantPaused, model)
			}
		})
	}
}
