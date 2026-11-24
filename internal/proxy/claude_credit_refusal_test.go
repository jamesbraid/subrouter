package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

const claudeCodeIdentityBody = `{"model":"claude-sonnet-5-5","max_tokens":20,"system":"You are Claude Code, Anthropic's official CLI for Claude.","messages":[{"role":"user","content":"Say ok"}]}`

func claudeCreditRefusalRequest(t *testing.T, apiKey, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	return req
}

// On 2026-11-23 eight funded keys refused, with "credit balance is too low",
// every request whose system prompt was the Claude Code identity line, and
// accepted it without the line. Failover walked each such request through
// every key and cooled them all. A second refusal of the same request must
// stop the walk, return the first key to routing, and explain the refusal.
func TestClaudeCreditRefusalThatFollowsTheRequestStopsFailover(t *testing.T) {
	server := Server{
		Accounts: []accounts.Account{
			{ID: "claude:a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-a"},
			{ID: "claude:b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-b"},
			{ID: "claude:c", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-c"},
		},
		Sessions:     newSessionStore(t),
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	hits := map[string]int{}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		hits[req.Header.Get("X-Api-Key")]++
		body, _ := io.ReadAll(req.Body)
		if claudeSystemIdentified(body) {
			header := http.Header{}
			header.Set("Request-Id", "req_refused")
			return &http.Response{StatusCode: http.StatusBadRequest, Header: header,
				Body: io.NopCloser(strings.NewReader(anthropicCreditBalanceBody))}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"ok"}`))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-identity", account: "claude:a",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
		budget: newAttemptBudget(5),
	}
	response, err := transport.RoundTrip(claudeCreditRefusalRequest(t, "tok-a", claudeCodeIdentityBody))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var envelope struct {
		Error struct{ Type, Message string }
	}
	_ = json.Unmarshal(raw, &envelope)
	if response.StatusCode != http.StatusBadRequest || response.Header.Get("X-Subrouter-Error") != "claude_request_credit_refusal" ||
		response.Header.Get("X-Should-Retry") != "false" || response.Header.Get("Request-Id") != "req_refused" ||
		!strings.Contains(envelope.Error.Message, "identifies itself as Claude Code") {
		t.Fatalf("response = %d %v %s, want subrouter's non-retryable identity explanation", response.StatusCode, response.Header, raw)
	}
	if total := hits["tok-a"] + hits["tok-b"] + hits["tok-c"]; total != 2 {
		t.Fatalf("hits = %v, want the walk to stop after the second key", hits)
	}
	for _, id := range []string{"claude:a", "claude:b", "claude:c"} {
		if _, held := server.SchedulerRef.CreditExhaustedUntil(accounts.ProviderClaude, id, time.Now()); held {
			t.Fatalf("%s is cooled down by a refusal the request caused", id)
		}
	}

	// The keys stay usable for requests their credit covers.
	plain := `{"model":"claude-sonnet-5-5","max_tokens":20,"messages":[{"role":"user","content":"Say ok"}]}`
	transport.budget = newAttemptBudget(5)
	ok, err := transport.RoundTrip(claudeCreditRefusalRequest(t, "tok-a", plain))
	if err != nil {
		t.Fatal(err)
	}
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("plain request after the refusal = %d, want 200", ok.StatusCode)
	}
}

func TestClaudeSystemIdentifiedMatchesOnlyAnExactFirstBlock(t *testing.T) {
	for _, tc := range []struct {
		system string
		want   bool
	}{
		{`"You are Claude Code, Anthropic's official CLI for Claude."`, true},
		{`[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Review."}]`, true},
		{`"You are a Claude agent, built on Anthropic's Claude Agent SDK."`, true},
		{`"You are Claude Code, Anthropic's official CLI for Claude.\n\nReview carefully."`, false},
		{`[{"type":"text","text":"Review."},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]`, false},
		{`"You are a careful reviewer."`, false},
	} {
		if got := claudeSystemIdentified([]byte(`{"system":` + tc.system + `}`)); got != tc.want {
			t.Fatalf("system %s identified = %v, want %v", tc.system, got, tc.want)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14, claude-code-20250219")
	if !claudeCodeIdentified(req) {
		t.Fatal("the claude-code beta header did not identify the request")
	}
}
