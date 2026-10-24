package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestClaudePoolExhaustedRetryAfterUsesEnabledAccountsAcrossPriorityTiers(t *testing.T) {
	disabled := accounts.Account{ID: "disabled", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	high := accounts.Account{ID: "high", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	low := accounts.Account{ID: "low", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	server, policies := accountPolicyRoutingServer(t, disabled, high, low)
	for id, policy := range map[string]accounts.AccountPolicy{
		disabled.ID: {Enabled: false},
		high.ID:     {Enabled: true, Priority: 10},
		low.ID:      {Enabled: true, Priority: -10},
	} {
		if err := policies.Update(accounts.ProviderClaude, id, policy); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	var scores []selectacct.Score
	for id, reset := range map[string]time.Duration{disabled.ID: 5 * time.Minute, high.ID: time.Hour, low.ID: 20 * time.Minute} {
		scores = append(scores, scoreFromUsageWindows(accounts.ProviderClaude, id, []accounts.UsageWindow{
			fiveHour(100, now.Add(reset)), sevenDay(10, now.Add(72*time.Hour)),
		}))
	}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	transport := usageLimitRetryTransport{server: server, provider: accounts.ProviderClaude}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	response, ok := transport.claudePoolExhaustedResponse(nil, req, high.ID, "max_attempts")
	if !ok || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("pool response=%v ok=%v, want 503", response, ok)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	result := httptest.NewRecorder()
	result.Header().Set("Retry-After", response.Header.Get("Retry-After"))
	if _, err := io.Copy(result.Body, response.Body); err != nil {
		t.Fatal(err)
	}
	requireRetryAfter(t, result, 20*time.Minute)
}

func TestClaudePoolExhaustedPolicyFailurePreservesUpstreamResponse(t *testing.T) {
	server, policies := accountPolicyRoutingServer(t, accounts.Account{ID: "plan", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth})
	if err := os.WriteFile(policies.Path(), []byte("invalid JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("upstream limit"))}
	t.Cleanup(func() { _ = original.Body.Close() })
	transport := usageLimitRetryTransport{server: server, provider: accounts.ProviderClaude}
	response, ok := transport.claudePoolExhaustedResponse(original, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), "plan", "max_attempts")
	if ok || response != nil {
		t.Fatalf("pool response=%v ok=%v, want original upstream response retained", response, ok)
	}
	body, err := io.ReadAll(original.Body)
	if err != nil || string(body) != "upstream limit" {
		t.Fatalf("upstream body=%q err=%v, want preserved limit response", body, err)
	}
}
