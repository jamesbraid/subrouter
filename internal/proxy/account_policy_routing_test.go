package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func accountPolicyRoutingServer(t *testing.T, items ...accounts.Account) (*Server, *accounts.PolicyStore) {
	t.Helper()
	store := accounts.CodexStore{Dir: t.TempDir()}
	policies, err := accounts.NewPolicyStore(filepath.Join(store.StoreDir(), "account-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	scores := make([]selectacct.Score, 0, len(items))
	for _, item := range items {
		scores = append(scores, selectacct.Score{
			AccountID: item.ID, Provider: item.Provider, Headroom: 1, ShortHeadroom: 1,
		})
	}
	return &Server{
		AccountRef:   NewAccountRef(store, items, nil),
		Sessions:     mustPolicyRoutingSessionStore(t),
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
	}, policies
}

func mustPolicyRoutingSessionStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func policyRoutingAccount(id string) accounts.Account {
	return accounts.Account{ID: id, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token-" + id}
}

func TestAccountPolicyDisabledAccountIsExcludedFromRouting(t *testing.T) {
	disabled := policyRoutingAccount("disabled@example.com")
	enabled := policyRoutingAccount("enabled@example.com")
	server, policies := accountPolicyRoutingServer(t, disabled, enabled)
	if err := policies.Update(disabled.Provider, disabled.ID, accounts.AccountPolicy{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: disabled.ID, Provider: disabled.Provider, Headroom: 1, ShortHeadroom: 1},
		{AccountID: enabled.ID, Provider: enabled.Provider, Headroom: 0.1, ShortHeadroom: 0.1},
	}))

	got, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "new-session", httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != enabled.ID {
		t.Fatalf("selected account = %q, want enabled account %q", got.ID, enabled.ID)
	}
}

func TestAccountPolicyTenantLeaseExcludesDisabledAccount(t *testing.T) {
	disabled := policyRoutingAccount("disabled@example.com")
	enabled := policyRoutingAccount("enabled@example.com")
	server, policies := accountPolicyRoutingServer(t, disabled, enabled)
	if err := policies.Update(disabled.Provider, disabled.ID, accounts.AccountPolicy{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: disabled.ID, Provider: disabled.Provider, Headroom: 1, ShortHeadroom: 1},
		{AccountID: enabled.ID, Provider: enabled.Provider, Headroom: 0.1, ShortHeadroom: 0.1},
	}))

	got, _, err := selectTenantCredentialLeaseAccount(t.Context(), nil, server, accounts.ProviderCodex, "", tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex", SessionID: "lease-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != enabled.ID {
		t.Fatalf("leased account = %q, want enabled account %q", got.ID, enabled.ID)
	}
}

func TestAccountPolicyTenantLeaseFallsThroughBlockedPriorityTier(t *testing.T) {
	high := policyRoutingAccount("high@example.com")
	low := policyRoutingAccount("low@example.com")
	server, policies := accountPolicyRoutingServer(t, high, low)
	if err := policies.Update(high.Provider, high.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	server.SchedulerRef.MarkExhaustedUntil(high.Provider, high.ID, tenantCredentialLeaseUnspecifiedModelPool, time.Now().Add(time.Hour))

	got, _, err := selectTenantCredentialLeaseAccount(t.Context(), nil, server, accounts.ProviderCodex, "", tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex", SessionID: "lease-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != low.ID {
		t.Fatalf("leased account = %q, want available lower-priority account %q", got.ID, low.ID)
	}
}

func TestAccountPolicyDisabledStickyAccountIsReassigned(t *testing.T) {
	disabled := policyRoutingAccount("disabled@example.com")
	enabled := policyRoutingAccount("enabled@example.com")
	server, policies := accountPolicyRoutingServer(t, disabled, enabled)
	if err := policies.Update(disabled.Provider, disabled.ID, accounts.AccountPolicy{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Sessions.Put("codex", "sticky-session", disabled.ID, ""); err != nil {
		t.Fatal(err)
	}

	got, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "sticky-session", httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != enabled.ID {
		t.Fatalf("selected account = %q, want reassigned account %q", got.ID, enabled.ID)
	}
	assignment, ok := server.Sessions.Get("codex", "sticky-session")
	if !ok || assignment.AccountID != enabled.ID {
		t.Fatalf("sticky assignment = %+v, want %q", assignment, enabled.ID)
	}
}

func TestAccountPolicyPriorityChangeKeepsStickyAccount(t *testing.T) {
	high := policyRoutingAccount("high@example.com")
	sticky := policyRoutingAccount("sticky@example.com")
	server, policies := accountPolicyRoutingServer(t, high, sticky)
	if err := policies.Update(high.Provider, high.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Sessions.Put("codex", "sticky-session", sticky.ID, ""); err != nil {
		t.Fatal(err)
	}

	got, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "sticky-session", httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sticky.ID {
		t.Fatalf("selected account = %q, want existing sticky account %q", got.ID, sticky.ID)
	}
}

func TestAccountPolicyTenantLeasePriorityChangeKeepsStickyAccount(t *testing.T) {
	high := policyRoutingAccount("high@example.com")
	sticky := policyRoutingAccount("sticky@example.com")
	server, policies := accountPolicyRoutingServer(t, high, sticky)
	if err := policies.Update(high.Provider, high.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Sessions.Put("codex", "sticky-session", sticky.ID, ""); err != nil {
		t.Fatal(err)
	}

	got, _, err := selectTenantCredentialLeaseAccount(t.Context(), nil, server, accounts.ProviderCodex, "", tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex", SessionID: "sticky-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sticky.ID {
		t.Fatalf("leased account = %q, want existing sticky account %q", got.ID, sticky.ID)
	}
}

func TestAccountPolicyForcedDisabledAccountFails(t *testing.T) {
	disabled := policyRoutingAccount("disabled@example.com")
	enabled := policyRoutingAccount("enabled@example.com")
	server, policies := accountPolicyRoutingServer(t, disabled, enabled)
	if err := policies.Update(disabled.Provider, disabled.ID, accounts.AccountPolicy{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	request.Header.Set("X-Subrouter-Account-ID", disabled.ID)

	_, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "forced-session", request)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("forced disabled account error = %v, want unavailable error", err)
	}
}

func TestAccountPolicyForcedEnabledLowerPriorityAccountIsSelectable(t *testing.T) {
	high := policyRoutingAccount("high@example.com")
	forced := policyRoutingAccount("forced@example.com")
	server, policies := accountPolicyRoutingServer(t, high, forced)
	if err := policies.Update(high.Provider, high.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	request.Header.Set("X-Subrouter-Account-ID", forced.ID)

	got, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "forced-session", request)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != forced.ID {
		t.Fatalf("selected account = %q, want forced lower-priority account %q", got.ID, forced.ID)
	}
}

func TestAccountPolicyTenantLeaseForcedEnabledLowerPriorityAccountIsSelectable(t *testing.T) {
	high := policyRoutingAccount("high@example.com")
	forced := policyRoutingAccount("forced@example.com")
	server, policies := accountPolicyRoutingServer(t, high, forced)
	if err := policies.Update(high.Provider, high.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
		t.Fatal(err)
	}

	got, _, err := selectTenantCredentialLeaseAccount(t.Context(), nil, server, accounts.ProviderCodex, "", tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex", SessionID: "forced-session", ForceAccountID: forced.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != forced.ID {
		t.Fatalf("leased account = %q, want forced lower-priority account %q", got.ID, forced.ID)
	}
}

func TestAccountPolicyPreferredAccountMustBeInHighestPriorityTier(t *testing.T) {
	high := policyRoutingAccount("high@example.com")
	preferred := policyRoutingAccount("preferred@example.com")
	server, policies := accountPolicyRoutingServer(t, high, preferred)
	if err := policies.Update(high.Provider, high.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))

	got, _, _, err := server.accountForSessionProviderWithOptions(accounts.ProviderCodex, "codex", "preferred-session", request, accountSelectionOptions{preferredAccountID: preferred.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != high.ID {
		t.Fatalf("selected account = %q, want highest-priority account %q", got.ID, high.ID)
	}
}

func TestAccountPolicyPriorityPrecedesSchedulerOrdering(t *testing.T) {
	high := policyRoutingAccount("high@example.com")
	low := policyRoutingAccount("low@example.com")
	server, policies := accountPolicyRoutingServer(t, high, low)
	if err := policies.Update(high.Provider, high.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
		t.Fatal(err)
	}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: high.ID, Provider: high.Provider, Headroom: 0.1, ShortHeadroom: 0.1},
		{AccountID: low.ID, Provider: low.Provider, Headroom: 1, ShortHeadroom: 1},
	}))

	got, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "priority-session", httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != high.ID {
		t.Fatalf("selected account = %q, want highest-priority account %q", got.ID, high.ID)
	}
}

func TestAccountPolicyRetryDescendsAfterHigherPriorityAccountsAreTried(t *testing.T) {
	highA := policyRoutingAccount("high-a@example.com")
	highB := policyRoutingAccount("high-b@example.com")
	low := policyRoutingAccount("low@example.com")
	server, policies := accountPolicyRoutingServer(t, highA, highB, low)
	for _, account := range []accounts.Account{highA, highB} {
		if err := policies.Update(account.Provider, account.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := server.retryAccount(t.Context(), accounts.ProviderCodex, "codex", "retry-session", "", map[string]struct{}{
		highA.ID: {},
		highB.ID: {},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != low.ID {
		t.Fatalf("retry account = %q, want lower-priority eligible account %q", got.ID, low.ID)
	}
}

func TestAccountPolicyOAuthRetryDescendsAfterHigherPriorityAccountsAreTried(t *testing.T) {
	highA := policyRoutingAccount("high-a@example.com")
	highB := policyRoutingAccount("high-b@example.com")
	low := policyRoutingAccount("low@example.com")
	server, policies := accountPolicyRoutingServer(t, highA, highB, low)
	for _, account := range []accounts.Account{highA, highB} {
		if err := policies.Update(account.Provider, account.ID, accounts.AccountPolicy{Enabled: true, Priority: 10}); err != nil {
			t.Fatal(err)
		}
	}
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, nil
	}

	got, err := server.oauthRetryCandidate(t.Context(), accounts.ProviderCodex, "codex", "retry-session", "", "", map[string]struct{}{
		highA.ID: {},
		highB.ID: {},
	}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != low.ID {
		t.Fatalf("OAuth retry account = %q, want lower-priority eligible account %q", got.ID, low.ID)
	}
}

func TestAccountPolicyDefaultKeepsSchedulerRouting(t *testing.T) {
	first := policyRoutingAccount("first@example.com")
	second := policyRoutingAccount("second@example.com")
	server, _ := accountPolicyRoutingServer(t, first, second)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: first.ID, Provider: first.Provider, Headroom: 0.1, ShortHeadroom: 0.1},
		{AccountID: second.ID, Provider: second.Provider, Headroom: 1, ShortHeadroom: 1},
	}))

	got, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", "default-session", httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != second.ID {
		t.Fatalf("selected account = %q, want scheduler choice %q", got.ID, second.ID)
	}
}
