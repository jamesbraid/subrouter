package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestClaudePoolSelectionRecoversFromMissingOrStaleModelScore(t *testing.T) {
	for _, tc := range []struct {
		model     string
		pool      string
		stalePool bool
		blocked   bool
	}{
		{model: "claude-opus-5-5", pool: "claudeopus"},
		{model: "claude-opus-5-5", pool: "claudeopus", stalePool: true},
		{model: "claude-opus-5-5", pool: "claudeopus", blocked: true},
		{model: "claude-sonnet-5", pool: "claudesonnet"},
		{model: "claude-sonnet-5", pool: "claudesonnet", stalePool: true},
	} {
		name := tc.model
		if tc.stalePool {
			name += "/stale-model-score"
		}
		if tc.blocked {
			name += "/explicit-reset-mark"
		}
		t.Run(name, func(t *testing.T) {
			testClaudePoolSelectionRecoversFromMissingOrStaleModelScore(t, tc.model, tc.pool, tc.stalePool, tc.blocked)
		})
	}
}

func testClaudePoolSelectionRecoversFromMissingOrStaleModelScore(t *testing.T, model, pool string, stalePool, blocked bool) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	recovered := selectacct.Score{AccountID: "recovered@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1, Fresh: true}
	if stalePool {
		recovered.ModelScores = map[string]selectacct.Score{pool: {AccountID: recovered.AccountID, Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0}}
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		recovered,
		{AccountID: "limited@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, Fresh: true},
		{AccountID: "dead@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, Fresh: false, CredentialUnavailable: true,
			ModelScores: map[string]selectacct.Score{pool: {AccountID: "dead@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, Fresh: false}}},
	}))
	if blocked {
		ref.MarkExhaustedUntil(accounts.ProviderClaude, recovered.AccountID, pool, time.Now().Add(time.Hour))
	}
	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "recovered@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-recovered"},
			{ID: "limited@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-limited"},
			{ID: "dead@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-dead"},
		},
		Sessions: store, SchedulerRef: ref, UsageScoreTTL: 0, MaxBodyBytes: 1 << 20,
	}.Handler()
	server := httptest.NewServer(handler)
	defer server.Close()
	request := func(id string, forced bool) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("X-Subrouter-Session", id)
		if forced {
			req.Header.Set("X-Subrouter-Account-ID", "recovered@example.com")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if status, body := request("forced-proof", true); status != http.StatusOK {
		t.Fatalf("forced proof status=%d body=%s", status, body)
	}
	wantStatus := http.StatusOK
	if blocked {
		wantStatus = http.StatusServiceUnavailable
	}
	if status, body := request("pooled-request", false); status != wantStatus {
		t.Fatalf("pooled request status=%d body=%s, want %d", status, body, wantStatus)
	}
}

func TestClaudePoolSelectionRecoversFromStaleAccountScore(t *testing.T) {
	for _, tc := range []struct {
		name         string
		model        string
		pool         string
		fresh        bool
		blocked      bool
		unavailable  bool
		incompatible bool
		dead         bool
		deadPeer     bool
		want         int
	}{
		{name: "opus stale account score", model: "claude-opus-5-5", pool: "claudeopus", want: http.StatusOK},
		{name: "sonnet stale account score", model: "claude-sonnet-5", pool: "claudesonnet", want: http.StatusOK},
		{name: "stale live account beside dead credential", model: "claude-opus-5-5", pool: "claudeopus", deadPeer: true, want: http.StatusOK},
		{name: "fresh measured exhaustion", model: "claude-opus-5-5", pool: "claudeopus", fresh: true, want: http.StatusServiceUnavailable},
		{name: "explicit reset mark", model: "claude-opus-5-5", pool: "claudeopus", blocked: true, want: http.StatusServiceUnavailable},
		{name: "unavailable account", model: "claude-opus-5-5", pool: "claudeopus", unavailable: true, want: http.StatusServiceUnavailable},
		{name: "incompatible model", model: "claude-opus-5-5", pool: "claudeopus", incompatible: true, want: http.StatusServiceUnavailable},
		{name: "dead credential", model: "claude-opus-5-5", pool: "claudeopus", dead: true, want: http.StatusServiceUnavailable},
		{name: "fable remains separate", model: "claude-fable", pool: "claudefable", want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.deadPeer && strings.Contains(r.Header.Get("Authorization"), "tok-dead") {
					t.Error("dead credential was sent upstream instead of the stale live account")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
			}))
			defer upstream.Close()
			upstreamURL, _ := url.Parse(upstream.URL)
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			scores := []selectacct.Score{
				{AccountID: "recovered@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, Fresh: tc.fresh, CredentialUnavailable: tc.dead},
				{AccountID: "limited@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, Fresh: true,
					ModelScores: map[string]selectacct.Score{tc.pool: {AccountID: "limited@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, Fresh: true}}},
			}
			if tc.deadPeer {
				scores = append(scores, selectacct.Score{AccountID: "aaa-dead@example.com", Provider: accounts.ProviderClaude,
					Headroom: 0, ShortHeadroom: 0, CredentialUnavailable: true})
			}
			ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
			if tc.blocked {
				ref.MarkExhaustedUntil(accounts.ProviderClaude, "recovered@example.com", tc.pool, time.Now().Add(time.Hour))
			}
			if tc.unavailable {
				ref.MarkAccountUnavailableUntil(accounts.ProviderClaude, "recovered@example.com", time.Now().Add(time.Hour))
			}
			if tc.incompatible {
				ref.MarkModelIncompatibleUntil(accounts.ProviderClaude, "recovered@example.com", tc.pool, time.Now().Add(time.Hour))
			}
			accountList := []accounts.Account{
				{ID: "recovered@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-recovered"},
				{ID: "limited@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-limited"},
			}
			if tc.deadPeer {
				accountList = append(accountList, accounts.Account{ID: "aaa-dead@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-dead"})
			}
			handler := Server{
				ClaudeUpstream: upstreamURL,
				Accounts:       accountList,
				Sessions:       store, SchedulerRef: ref, UsageScoreTTL: 0, MaxBodyBytes: 1 << 20,
			}.Handler()
			server := httptest.NewServer(handler)
			defer server.Close()
			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"`+tc.model+`","messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Subrouter-Agent", "claude")
			req.Header.Set("X-Subrouter-Session", "pooled-request")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want {
				t.Fatalf("pooled request status=%d body=%s, want %d", resp.StatusCode, body, tc.want)
			}
		})
	}
}

func TestClaudeStaleAccountProbeRespectsPriority(t *testing.T) {
	base := selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "preferred", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		{AccountID: "lower", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	})
	ref := selectacct.NewSchedulerRef(base)
	server := Server{SchedulerRef: ref}
	policies := []accounts.AccountWithPolicy{
		{Account: accounts.Account{ID: "lower", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}, Policy: accounts.AccountPolicy{Enabled: true, Priority: 1}},
		{Account: accounts.Account{ID: "preferred", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}, Policy: accounts.AccountPolicy{Enabled: true, Priority: 2}},
		{Account: accounts.Account{ID: "disabled", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}, Policy: accounts.AccountPolicy{Enabled: false, Priority: 3}},
	}
	got, ok := server.pickClaudeModelQuotaProbe("claude-opus", base, accounts.FilterEnabled(policies))
	if !ok || got.ID != "preferred" {
		t.Fatalf("stale account probe = %q, ok=%t, want preferred tier", got.ID, ok)
	}
}
