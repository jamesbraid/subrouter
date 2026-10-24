package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestAccountPolicyPriorityPreservesClaudeSubscriptionCostOrder(t *testing.T) {
	for _, mode := range []string{"new", "legacy-key-sticky", "preferred-key", "retry", "forced-key"} {
		t.Run(mode, func(t *testing.T) {
			subscription := accounts.Account{ID: "subscription", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "subscription-token"}
			key := accounts.Account{ID: "claude:key", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "key-token"}
			server, policies := accountPolicyRoutingServer(t, subscription, key)
			server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				return account, nil
			}
			if err := policies.Update(key.Provider, key.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
				t.Fatal(err)
			}
			if mode == "legacy-key-sticky" {
				if _, err := server.Sessions.Put("claude", "cost-order", key.ID, ""); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
			if mode == "forced-key" {
				req.Header.Set("X-Subrouter-Account-ID", key.ID)
			}
			var got accounts.Account
			var err error
			if mode == "retry" {
				got, err = server.oauthRetryCandidate(t.Context(), accounts.ProviderClaude, "claude", "cost-order", "", "claude-opus", nil, false, false)
			} else {
				options := accountSelectionOptions{}
				if mode == "preferred-key" {
					options.preferredAccountID = key.ID
				}
				got, _, _, err = server.accountForSessionProviderWithOptions(accounts.ProviderClaude, "claude", "cost-order", req, options)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := subscription.ID
			if mode == "forced-key" {
				want = key.ID
			}
			if got.ID != want {
				t.Fatalf("selected %q, want %q", got.ID, want)
			}
		})
	}
}

func TestAccountPolicyDisabledClaudeKeyDoesNotBlockExtraUsage(t *testing.T) {
	paid := accounts.Account{ID: "paid", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "paid-token"}
	key := accounts.Account{ID: "claude:key", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "key-token"}
	server, policies := accountPolicyRoutingServer(t, paid, key)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: paid.ID, Provider: paid.Provider, Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true,
			ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 9},
		{AccountID: key.ID, Provider: key.Provider, Headroom: 1, ShortHeadroom: 1},
	}))
	if server.claudeExtraUsageResponseAllowed(t.Context(), paid.ID, "") {
		t.Fatal("extra usage was allowed while an enabled API key could serve the request")
	}
	if err := policies.Update(key.Provider, key.ID, accounts.AccountPolicy{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if !server.claudeExtraUsageResponseAllowed(t.Context(), paid.ID, "") {
		t.Fatal("disabled API key blocked authorized Claude extra usage")
	}
}
