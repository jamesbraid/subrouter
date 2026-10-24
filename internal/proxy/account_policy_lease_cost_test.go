package proxy

import (
	"context"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAccountPolicyClaudeLeasePreservesSubscriptionCostOrder(t *testing.T) {
	for _, tc := range []struct {
		name             string
		requiredAuthMode accounts.AuthMode
		preferKey        bool
		forceKey         bool
		stickyKey        bool
		disablePlan      bool
		wantKey          bool
	}{
		{name: "new lease uses subscription"},
		{name: "preferred key does not preempt subscription", preferKey: true},
		{name: "required API key remains eligible", requiredAuthMode: accounts.AuthModeAPIKey, wantKey: true},
		{name: "forced key remains pinned", forceKey: true, wantKey: true},
		{name: "sticky key remains pinned", stickyKey: true, wantKey: true},
		{name: "disabled subscription falls back to key", disablePlan: true, wantKey: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := accounts.Account{ID: "plan", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "plan-token"}
			key := accounts.Account{ID: "claude:key", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "key-token"}
			server, policies := accountPolicyRoutingServer(t, plan, key)
			server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				return account, nil
			}
			if err := policies.Update(key.Provider, key.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
				t.Fatal(err)
			}
			if tc.disablePlan {
				if err := policies.Update(plan.Provider, plan.ID, accounts.AccountPolicy{Enabled: false}); err != nil {
					t.Fatal(err)
				}
			}
			input := tenantCredentialLeaseRequest{Provider: string(accounts.ProviderClaude), AgentType: "claude", SessionID: "lease-cost", Model: "claude-opus-4-8"}
			if tc.preferKey {
				input.PreferAccountID = key.ID
			}
			if tc.forceKey {
				input.ForceAccountID = key.ID
			}
			if tc.stickyKey {
				if _, err := server.Sessions.Put("claude", input.SessionID, key.ID, ""); err != nil {
					t.Fatal(err)
				}
			}
			got, _, placement, err := selectTenantCredentialLeaseAccount(t.Context(), nil, server, accounts.ProviderClaude, tc.requiredAuthMode, input)
			if placement != nil {
				t.Cleanup(placement.Release)
			}
			if err != nil {
				t.Fatal(err)
			}
			want := plan.ID
			if tc.wantKey {
				want = key.ID
			}
			if got.ID != want {
				t.Fatalf("leased %q, want %q", got.ID, want)
			}
		})
	}
}
