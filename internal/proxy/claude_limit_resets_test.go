package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestClaudeLimitResetStatusReportsScopedUnknownAndFencesCredentials(t *testing.T) {
	account := accounts.Account{ID: "claude-test", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
	ref.rememberClaudeUsageScopeDenied(account)
	if got := ref.claudeLimitResetStatus(account); got.Status != "scope_required" || got.Known {
		t.Fatalf("scope metadata without status fetch=%+v", got)
	}
	client := &http.Client{Transport: proxyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected scoped read %s", r.URL)
		}
		h := make(http.Header)
		h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.03")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"message"}`)), Request: r}, nil
	})}
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	status := ref.claudeLimitResetStatus(account)
	if status.Known || status.Status != "scope_required" {
		t.Fatalf("status=%+v", status)
	}
	ref.usageStatusCache = []AccountUsageStatus{{AccountStatus: AccountStatus{ID: account.ID, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}}}
	ref.usageStatusAt = time.Now()
	response := httptest.NewRecorder()
	Server{AccountRef: ref}.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/_subrouter/usage-status?snapshot=1", nil))
	var rows []AccountUsageStatus
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].LimitResets == nil || rows[0].LimitResets.Known || rows[0].LimitResets.Status != "scope_required" {
		t.Fatalf("JSON=%s", response.Body.String())
	}
	account.Token = "replacement"
	if got := ref.claudeLimitResetStatus(account); got.Known || got.Status != "unavailable" {
		t.Fatalf("prior credential report leaked: %+v", got)
	}
}
