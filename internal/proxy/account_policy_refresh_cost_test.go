package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestAccountPolicyRefreshFailoverPreservesClaudeSubscriptionCostOrder(t *testing.T) {
	dead := accounts.Account{ID: "dead", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "dead-token"}
	subscription := accounts.Account{ID: "subscription", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "subscription-token"}
	key := accounts.Account{ID: "claude:key", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "key-token"}
	server, policies := accountPolicyRoutingServer(t, dead, subscription, key)
	if err := policies.Update(key.Provider, key.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	var refreshed []string
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		refreshed = append(refreshed, account.ID)
		if account.ID == dead.ID {
			return account, errors.New("Claude OAuth refresh failed: invalid_grant")
		}
		return account, nil
	}
	got, pending, err := server.refreshSelectedAccount(t.Context(), accounts.ProviderClaude, "claude", "cost-order", "", claudeRefreshRequest(t), dead)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != subscription.ID {
		t.Fatalf("selected %q after credential failure, want included subscription %q", got.ID, subscription.ID)
	}
	if !pending {
		t.Fatal("alternate assignment was committed before upstream success")
	}
	if len(refreshed) != 2 || refreshed[0] != dead.ID || refreshed[1] != subscription.ID {
		t.Fatalf("refreshed accounts = %v, want dead account then included subscription", refreshed)
	}
}

func TestAccountPolicyExtraUsagePriorityAfterWholePoolEligibility(t *testing.T) {
	for _, tc := range []struct {
		name         string
		priorityPaid bool
		priorityOff  bool
		priorityHeld bool
		balanceLive  bool
		balanceOff   bool
		noAccountRef bool
		noController bool
		want         string
	}{
		{name: "priority before balance", priorityPaid: true, want: "priority"},
		{name: "priority without controller", priorityPaid: true, noController: true, want: "priority"},
		{name: "unfunded priority falls through", want: "balance"},
		{name: "lower priority subscription has quota", priorityPaid: true, balanceLive: true},
		{name: "disabled priority falls through", priorityPaid: true, priorityOff: true, want: "balance"},
		{name: "disabled healthy subscription cannot block paid", priorityPaid: true, balanceLive: true, balanceOff: true, want: "priority"},
		{name: "held priority falls through", priorityPaid: true, priorityHeld: true, want: "balance"},
		{name: "default policies keep balance order", priorityPaid: true, noAccountRef: true, want: "balance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			priority := accounts.Account{ID: "priority", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "priority-token"}
			balance := accounts.Account{ID: "balance", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "balance-token"}
			server, policies := accountPolicyRoutingServer(t, priority, balance)
			if err := policies.Update(priority.Provider, priority.ID, accounts.AccountPolicy{Enabled: !tc.priorityOff, Priority: 10}); err != nil {
				t.Fatal(err)
			}
			if err := policies.Update(balance.Provider, balance.ID, accounts.AccountPolicy{Enabled: !tc.balanceOff}); err != nil {
				t.Fatal(err)
			}
			priorityRemaining := 0.0
			if tc.priorityPaid {
				priorityRemaining = 5
			}
			scores := []selectacct.Score{
				{AccountID: priority.ID, Provider: priority.Provider, Fresh: true, ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: priorityRemaining},
				{AccountID: balance.ID, Provider: balance.Provider, Fresh: true, ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 50},
			}
			if tc.balanceLive {
				scores[1].Headroom, scores[1].ShortHeadroom = 1, 1
			}
			scheduler := selectacct.NewScheduler(scores)
			server.SchedulerRef = selectacct.NewSchedulerRef(scheduler)
			if tc.priorityHeld {
				server.SchedulerRef.MarkAccountUnavailableUntil(priority.Provider, priority.ID, time.Now().Add(time.Hour))
			}
			if tc.noAccountRef {
				server.AccountRef = nil
			}
			if tc.noController {
				server.SchedulerRef = nil
			}
			got, ok := server.pickClaudeExtraUsageFallbackForRequest(scheduler, []accounts.Account{priority, balance}, "")
			if got.ID != tc.want || ok != (tc.want != "") {
				t.Fatalf("extra usage account = %q, available = %t, want account = %q", got.ID, ok, tc.want)
			}
		})
	}
}
