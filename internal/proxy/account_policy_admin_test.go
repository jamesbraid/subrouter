package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/session"
)

func TestAccountPolicyListIncludesProviderQualifiedPolicies(t *testing.T) {
	handler, ref, _, _, _ := newAccountPolicyAdminServer(t)
	if err := ref.policyStore.Update(accounts.ProviderCodex, "shared", accounts.AccountPolicy{Enabled: false, Priority: 17}); err != nil {
		t.Fatal(err)
	}
	if err := ref.policyStore.Update(accounts.ProviderClaude, "shared", accounts.AccountPolicy{Enabled: true, Priority: -4}); err != nil {
		t.Fatal(err)
	}

	response := serveAccountPolicyAdmin(handler, http.MethodGet, "/_subrouter/accounts", "", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var rows []struct {
		ID       string            `json:"id"`
		Provider accounts.Provider `json:"provider"`
		Enabled  bool              `json:"enabled"`
		Priority int               `json:"priority"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]struct {
		enabled  bool
		priority int
	})
	for _, row := range rows {
		got[string(row.Provider)+"/"+row.ID] = struct {
			enabled  bool
			priority int
		}{row.Enabled, row.Priority}
	}
	if got["codex/shared"] != (struct {
		enabled  bool
		priority int
	}{false, 17}) {
		t.Fatalf("Codex shared row = %#v, want disabled priority 17", got["codex/shared"])
	}
	if got["claude/shared"] != (struct {
		enabled  bool
		priority int
	}{true, -4}) {
		t.Fatalf("Claude shared row = %#v, want enabled priority -4", got["claude/shared"])
	}
	if strings.Contains(response.Body.String(), "codex-secret") || strings.Contains(response.Body.String(), "claude-access") {
		t.Fatalf("account list exposed credential material: %s", response.Body.String())
	}
}

func TestAccountPolicyCollectionIsGetOnly(t *testing.T) {
	handler, _, _, _, _ := newAccountPolicyAdminServer(t)
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			response := serveAccountPolicyAdmin(handler, method, "/_subrouter/accounts", `{}`, true)
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", response.Code)
			}
			if got := response.Header().Get("Allow"); got != http.MethodGet {
				t.Fatalf("Allow = %q, want GET", got)
			}
		})
	}
}

func TestAccountPolicyPatchPartiallyUpdatesOneProviderAndReloads(t *testing.T) {
	handler, ref, _, _, _ := newAccountPolicyAdminServer(t)
	path := "/_subrouter/accounts/codex/sh%61red"

	response := serveAccountPolicyAdmin(handler, http.MethodPatch, path, `{"enabled":false}`, true)
	if response.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200: %s", response.Code, response.Body.String())
	}
	policy, err := ref.policyStore.Load(accounts.ProviderCodex, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Enabled || policy.Priority != 0 {
		t.Fatalf("disabled policy = %+v, want enabled=false priority=0", policy)
	}

	response = serveAccountPolicyAdmin(handler, http.MethodPatch, path, `{"priority":23}`, true)
	if response.Code != http.StatusOK {
		t.Fatalf("priority status = %d, want 200: %s", response.Code, response.Body.String())
	}
	policy, err = ref.policyStore.Load(accounts.ProviderCodex, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Enabled || policy.Priority != 23 {
		t.Fatalf("updated policy = %+v, want enabled=false priority=23", policy)
	}
	decorated, _, err := Server{AccountRef: ref}.accountPolicySnapshotContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range decorated {
		if account.Account.Provider == accounts.ProviderCodex && account.Account.ID == "shared" && account.Policy != policy {
			t.Fatalf("live Codex policy = %+v, want %+v", account.Policy, policy)
		}
	}
}

func TestAccountPolicyPatchRejectsUnauthorizedAndInvalidInputWithoutMutation(t *testing.T) {
	handler, ref, _, _, _ := newAccountPolicyAdminServer(t)
	path := "/_subrouter/accounts/codex/shared"
	for _, tc := range []struct {
		name       string
		body       string
		authorized bool
		wantStatus int
	}{
		{name: "missing admin token", body: `{"enabled":false}`, wantStatus: http.StatusUnauthorized},
		{name: "invalid JSON", body: `{"enabled":`, authorized: true, wantStatus: http.StatusBadRequest},
		{name: "empty patch", body: `{}`, authorized: true, wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"other":true}`, authorized: true, wantStatus: http.StatusBadRequest},
		{name: "duplicate field", body: `{"enabled":true,"enabled":false}`, authorized: true, wantStatus: http.StatusBadRequest},
		{name: "fractional priority", body: `{"priority":1.5}`, authorized: true, wantStatus: http.StatusBadRequest},
		{name: "priority above range", body: `{"priority":1001}`, authorized: true, wantStatus: http.StatusBadRequest},
		{name: "priority below range", body: `{"priority":-1001}`, authorized: true, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := serveAccountPolicyAdmin(handler, http.MethodPatch, path, tc.body, tc.authorized)
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.wantStatus, response.Body.String())
			}
			policy, err := ref.policyStore.Load(accounts.ProviderCodex, "shared")
			if err != nil {
				t.Fatal(err)
			}
			if policy != (accounts.AccountPolicy{Enabled: true}) {
				t.Fatalf("invalid patch changed policy to %+v", policy)
			}
		})
	}

	for _, path := range []string{
		"/_subrouter/accounts/not-a-provider/shared",
		"/_subrouter/accounts/codex/missing",
	} {
		response := serveAccountPolicyAdmin(handler, http.MethodPatch, path, `{"enabled":false}`, true)
		if response.Code != http.StatusNotFound {
			t.Fatalf("path %q status = %d, want 404: %s", path, response.Code, response.Body.String())
		}
	}
}

func TestAccountPolicyDeleteErrorRecoversBeforeSameIDReenrollment(t *testing.T) {
	_, ref, codexStore, _, sessions := newAccountPolicyAdminServer(t)
	if err := ref.policyStore.Update(accounts.ProviderCodex, "shared", accounts.AccountPolicy{Enabled: false, Priority: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("codex", "stale", "shared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("claude", "collision", "shared", ""); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(codexStore.StoreDir(), "account-policy.json")
	originalPolicy, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := Server{AccountRef: ref, Sessions: sessions, AdminToken: "secret"}
	err = server.removeAccountPolicyCredential(t.Context(), accounts.ProviderCodex, "shared")
	if err == nil {
		t.Fatal("delete succeeded despite unreadable policy")
	}
	if _, found, journalErr := readAccountRollbackJournal(codexStore.StoreDir()); journalErr != nil || !found {
		t.Fatalf("durable deletion journal after cleanup error: found=%v err=%v", found, journalErr)
	}
	if err := os.WriteFile(policyPath, originalPolicy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileCompletedAccountRollback(t.Context(), codexStore, advanceAccountDiskGeneration); err != nil {
		t.Fatal(err)
	}
	if _, found, err := readAccountRollbackJournal(codexStore.StoreDir()); err != nil || found {
		t.Fatalf("journal remains: found=%v err=%v", found, err)
	}

	replacement := accounts.StoredCodexAccount{Email: "shared", Provider: accounts.ProviderCodex, Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "replacement"}}
	if err := codexStore.SaveStored(replacement); err != nil {
		t.Fatal(err)
	}
	policy, err := ref.policyStore.Load(accounts.ProviderCodex, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if policy != (accounts.AccountPolicy{Enabled: true}) {
		t.Fatalf("re-enrolled policy = %+v, want default", policy)
	}
	got := sessions.All()
	if len(got) != 1 || got[0].AgentType != "claude" || got[0].SessionID != "collision" {
		t.Fatalf("recovery crossed provider boundary or retained stale session: %#v", got)
	}
}

func TestAccountPolicyDeleteRecoversCrashAfterCredentialRemoval(t *testing.T) {
	_, ref, codexStore, _, sessions := newAccountPolicyAdminServer(t)
	if err := ref.policyStore.Update(accounts.ProviderCodex, "shared", accounts.AccountPolicy{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("codex", "stale", "shared", ""); err != nil {
		t.Fatal(err)
	}
	stored, found, err := codexStore.FindStored("shared")
	if err != nil || !found {
		t.Fatalf("find stored: found=%v err=%v", found, err)
	}
	cleanup := &accountPolicyDeletionCleanup{
		Provider: accounts.ProviderCodex, AccountID: "shared",
		PolicyStorePath: ref.policyStore.Path(), SessionStorePath: sessions.Path(),
	}
	journal, err := prepareStoredAccountPolicyDelete(codexStore.Dir, stored, cleanup)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := codexStore.AcquireStoredAccountLease("shared")
	if err != nil {
		t.Fatal(err)
	}
	removed, removeErr := replayPreparedTenantStoredRemovalWithLease(journal, lease)
	if closeErr := lease.Close(); removeErr == nil {
		removeErr = closeErr
	}
	if removeErr != nil || !removed {
		t.Fatalf("remove credential: removed=%v err=%v", removed, removeErr)
	}
	if err := markAccountRollbackRemoved(codexStore.StoreDir()); err != nil {
		t.Fatal(err)
	}

	if _, err := reconcileCompletedAccountRollback(t.Context(), codexStore, advanceAccountDiskGeneration); err != nil {
		t.Fatal(err)
	}
	policy, err := ref.policyStore.Load(accounts.ProviderCodex, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if policy != (accounts.AccountPolicy{Enabled: true}) {
		t.Fatalf("policy after crash recovery = %+v", policy)
	}
	if got := sessions.All(); len(got) != 0 {
		t.Fatalf("sessions after crash recovery = %#v", got)
	}
	if _, found, err := readAccountRollbackJournal(codexStore.StoreDir()); err != nil || found {
		t.Fatalf("journal remains: found=%v err=%v", found, err)
	}
}

func TestAccountPolicyClaudeDeleteRecoveryCleansPolicyBeforeSameIDReenrollment(t *testing.T) {
	_, ref, codexStore, claudeStore, sessions := newAccountPolicyAdminServer(t)
	if claudeStore.Dir != codexStore.StoreDir() {
		t.Fatalf("Claude store = %q, want production shared state dir %q", claudeStore.Dir, codexStore.StoreDir())
	}
	if err := ref.policyStore.Update(accounts.ProviderClaude, "shared", accounts.AccountPolicy{Enabled: false, Priority: 21}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("claude", "stale", "shared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("codex", "collision", "shared", ""); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := claudeStore.SnapshotProfileRemovalContext(t.Context(), "shared")
	if err != nil || !found {
		t.Fatalf("snapshot Claude profile: found=%v err=%v", found, err)
	}
	cleanup := &accountPolicyDeletionCleanup{
		Provider: accounts.ProviderClaude, AccountID: "shared",
		PolicyStorePath: ref.policyStore.Path(), SessionStorePath: sessions.Path(),
	}
	journal, found, err := prepareClaudeProfileDelete(t.Context(), codexStore.StoreDir(), "shared", claudeStore, snapshot, cleanup)
	if err != nil || !found {
		t.Fatalf("prepare Claude deletion: found=%v err=%v", found, err)
	}
	removed, err := replayPreparedClaudeProfileRollback(t.Context(), codexStore.StoreDir(), journal)
	if err != nil || !removed {
		t.Fatalf("remove Claude credential: removed=%v err=%v", removed, err)
	}
	if err := markAccountRollbackRemoved(codexStore.StoreDir()); err != nil {
		t.Fatal(err)
	}

	if _, err := reconcileCompletedAccountRollback(t.Context(), codexStore, advanceAccountDiskGeneration); err != nil {
		t.Fatal(err)
	}
	if err := claudeStore.ImportProfileCredential("shared", agentclaude.CredentialInfo{AccessToken: "replacement", RefreshToken: "replacement-refresh"}); err != nil {
		t.Fatal(err)
	}
	policy, err := ref.policyStore.Load(accounts.ProviderClaude, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if policy != (accounts.AccountPolicy{Enabled: true}) {
		t.Fatalf("re-enrolled Claude policy = %+v, want default", policy)
	}
	got := sessions.All()
	if len(got) != 1 || got[0].AgentType != "codex" || got[0].SessionID != "collision" {
		t.Fatalf("Claude recovery crossed provider boundary or retained stale session: %#v", got)
	}
}

func TestAccountPolicyPatchRejectsAmbiguousProviderQualifiedID(t *testing.T) {
	handler, ref, _, _, _ := newAccountPolicyAdminServer(t)
	ref.oauthSources = []OAuthAccountSource{accountPolicyTestSource{accounts: []accounts.Account{{
		ID: "shared", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth,
	}}}}

	response := serveAccountPolicyAdmin(handler, http.MethodPatch, "/_subrouter/accounts/codex/shared", `{"enabled":false}`, true)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
	}
	policy, err := ref.policyStore.Load(accounts.ProviderCodex, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if policy != (accounts.AccountPolicy{Enabled: true}) {
		t.Fatalf("ambiguous patch changed policy to %+v", policy)
	}
}

func TestAccountPolicyDeleteRemovesOnlyQualifiedCredentialPolicyAndSessions(t *testing.T) {
	handler, ref, codexStore, claudeStore, sessions := newAccountPolicyAdminServer(t)
	if err := ref.policyStore.Update(accounts.ProviderCodex, "shared", accounts.AccountPolicy{Enabled: false, Priority: 3}); err != nil {
		t.Fatal(err)
	}
	if err := ref.policyStore.Update(accounts.ProviderClaude, "shared", accounts.AccountPolicy{Enabled: true, Priority: 9}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("codex", "stale", "shared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("claude", "other-provider", "shared", ""); err != nil {
		t.Fatal(err)
	}

	response := serveAccountPolicyAdmin(handler, http.MethodDelete, "/_subrouter/accounts/codex/shared", "", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	stored, err := codexStore.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("Codex credential remains after deletion: %#v", stored)
	}
	claudeAccounts, err := claudeStore.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(claudeAccounts) != 1 || claudeAccounts[0].ID != "shared" {
		t.Fatalf("Claude collision was removed: %#v", claudeAccounts)
	}
	policies, err := ref.policyStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, found := policies[accounts.PolicyKey{Provider: accounts.ProviderCodex, AccountID: "shared"}]; found {
		t.Fatal("Codex policy remains after account deletion")
	}
	if got := policies[accounts.PolicyKey{Provider: accounts.ProviderClaude, AccountID: "shared"}]; got != (accounts.AccountPolicy{Enabled: true, Priority: 9}) {
		t.Fatalf("Claude policy = %+v, want enabled priority 9", got)
	}
	if got := sessions.All(); len(got) != 1 || got[0].AgentType != "claude" || got[0].SessionID != "other-provider" {
		t.Fatalf("session cleanup crossed provider boundary: %#v", got)
	}
	for _, account := range ref.All() {
		if account.Provider == accounts.ProviderCodex && account.ID == "shared" {
			t.Fatalf("deleted Codex account remains in live pool: %#v", ref.All())
		}
	}
}

func TestAccountPolicyDeleteClaudeKeepsSameIDCodexSession(t *testing.T) {
	handler, ref, _, claudeStore, sessions := newAccountPolicyAdminServer(t)
	if err := ref.policyStore.Update(accounts.ProviderClaude, "shared", accounts.AccountPolicy{Enabled: false, Priority: 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("codex", "codex-session", "shared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("claude", "claude-session", "shared", ""); err != nil {
		t.Fatal(err)
	}

	response := serveAccountPolicyAdmin(handler, http.MethodDelete, "/_subrouter/accounts/claude/shared", "", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	claudeAccounts, err := claudeStore.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(claudeAccounts) != 0 {
		t.Fatalf("Claude credential remains after deletion: %#v", claudeAccounts)
	}
	policy, err := ref.policyStore.Load(accounts.ProviderClaude, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if policy != (accounts.AccountPolicy{Enabled: true}) {
		t.Fatalf("Claude policy after deletion = %+v, want default", policy)
	}
	if got := sessions.All(); len(got) != 1 || got[0].AgentType != "codex" || got[0].SessionID != "codex-session" {
		t.Fatalf("Claude deletion removed Codex session with the same account ID: %#v", got)
	}
}

func TestAccountPolicyDeleteClaudeAPIKeyUsesStoredCredentialJournalAndRecovers(t *testing.T) {
	handler, ref, codexStore, _, sessions := newAccountPolicyAdminServer(t)
	stored, _, err := codexStore.AddProviderAPIKey(accounts.ProviderClaude, "ops", "sk-ant-ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ref.ReloadSnapshot(); err != nil {
		t.Fatal(err)
	}
	if err := ref.policyStore.Update(accounts.ProviderClaude, stored.Email, accounts.AccountPolicy{Enabled: false, Priority: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("claude", "stale-api-key", stored.Email, ""); err != nil {
		t.Fatal(err)
	}

	response := serveAccountPolicyAdmin(handler, http.MethodDelete, "/_subrouter/accounts/claude/claude:ops", "", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if _, found, err := codexStore.FindStored(stored.Email); err != nil || found {
		t.Fatalf("Claude API-key credential after deletion: found=%v err=%v", found, err)
	}
	if _, found, err := readAccountRollbackJournal(codexStore.StoreDir()); err != nil || found {
		t.Fatalf("recovery journal remains: found=%v err=%v", found, err)
	}
	policy, err := ref.policyStore.Load(accounts.ProviderClaude, stored.Email)
	if err != nil {
		t.Fatal(err)
	}
	if policy != (accounts.AccountPolicy{Enabled: true}) {
		t.Fatalf("Claude API-key policy after deletion = %+v", policy)
	}
	if _, found := sessions.Get("claude", "stale-api-key"); found {
		t.Fatal("Claude API-key session remained after deletion")
	}
}

func TestAccountPolicyDeletionJournalRejectsMixedBackendBeforeMutation(t *testing.T) {
	_, ref, codexStore, _, sessions := newAccountPolicyAdminServer(t)
	stored, _, err := codexStore.AddProviderAPIKey(accounts.ProviderClaude, "ops", "sk-ant-ops")
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.policyStore.Update(accounts.ProviderClaude, stored.Email, accounts.AccountPolicy{Enabled: false, Priority: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("claude", "stale-api-key", stored.Email, ""); err != nil {
		t.Fatal(err)
	}

	j, err := prepareStoredAccountPolicyDelete(codexStore.Dir, stored, &accountPolicyDeletionCleanup{
		Provider: accounts.ProviderClaude, AccountID: stored.Email,
		PolicyStorePath: ref.policyStore.Path(), SessionStorePath: sessions.Path(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.CredentialBackend != accountPolicyBackendStored {
		t.Fatalf("stored Claude API-key journal backend = %q, want %q", j.CredentialBackend, accountPolicyBackendStored)
	}
	j.CredentialBackend = accountPolicyBackendClaudeProfile
	j.TargetID = stored.Email
	if err := writeAccountRollbackJournal(codexStore.StoreDir(), j); err != nil {
		t.Fatal(err)
	}

	if _, err := reconcileCompletedAccountRollback(t.Context(), codexStore, advanceAccountDiskGeneration); err == nil {
		t.Fatal("mixed-backend journal was accepted")
	}
	if _, found, err := codexStore.FindStored(stored.Email); err != nil || !found {
		t.Fatalf("mixed-backend journal removed credential: found=%v err=%v", found, err)
	}
	policy, err := ref.policyStore.Load(accounts.ProviderClaude, stored.Email)
	if err != nil {
		t.Fatal(err)
	}
	if policy != (accounts.AccountPolicy{Enabled: false, Priority: 4}) {
		t.Fatalf("mixed-backend journal changed policy to %+v", policy)
	}
	if _, found := sessions.Get("claude", "stale-api-key"); !found {
		t.Fatal("mixed-backend journal removed session")
	}
}

func TestAccountPolicyDeleteRejectsUnsupportedProviderWithoutMutation(t *testing.T) {
	handler, ref, codexStore, _, _ := newAccountPolicyAdminServer(t)
	ref.mu.Lock()
	ref.accounts = append(ref.accounts, accounts.Account{
		ID: "grok:ops", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeOAuth, Token: "grok-token",
	})
	ref.mu.Unlock()

	response := serveAccountPolicyAdmin(handler, http.MethodDelete, "/_subrouter/accounts/grok/grok:ops", "", true)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "only Codex and Claude") {
		t.Fatalf("unsupported-provider response = %q", response.Body.String())
	}
	if _, found, err := codexStore.FindStored("shared"); err != nil || !found {
		t.Fatalf("unsupported removal changed stored credentials: found=%v err=%v", found, err)
	}
}

func newAccountPolicyAdminServer(t *testing.T) (http.Handler, *AccountRef, accounts.CodexStore, agentclaude.Store, *session.Store) {
	t.Helper()
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	if err := codexStore.SaveStored(accounts.StoredCodexAccount{
		Email: "shared", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "codex-secret"},
	}); err != nil {
		t.Fatal(err)
	}
	// Production keeps Claude profile state beside account-policy.json in the
	// shared Codex state directory, not in a provider-specific child.
	claudeStore := agentclaude.Store{Dir: codexStore.StoreDir()}
	if _, err := claudeStore.CreateProfile("shared"); err != nil {
		t.Fatal(err)
	}
	if err := claudeStore.ImportProfileCredential("shared", agentclaude.CredentialInfo{AccessToken: "claude-access", RefreshToken: "claude-refresh"}); err != nil {
		t.Fatal(err)
	}
	initial, err := loadAccountRefAccounts(codexStore, claudeStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(codexStore, initial, nil)
	ref.claudeStore = claudeStore
	sessions, err := session.NewStore(filepath.Join(root, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Server{AccountRef: ref, Sessions: sessions, AdminToken: "secret"}.Handler(), ref, codexStore, claudeStore, sessions
}

func serveAccountPolicyAdmin(handler http.Handler, method, path, body string, authorized bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.RemoteAddr = "100.64.0.20:4321"
	if authorized {
		request.Header.Set("Authorization", "Bearer secret")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type accountPolicyTestSource struct {
	accounts []accounts.Account
}

func (s accountPolicyTestSource) Provider() accounts.Provider { return accounts.ProviderCodex }

func (s accountPolicyTestSource) ListAccounts(context.Context) ([]accounts.Account, error) {
	return append([]accounts.Account(nil), s.accounts...), nil
}

func (accountPolicyTestSource) RefreshAccount(_ context.Context, _ *http.Client, account accounts.Account) (accounts.Account, error) {
	return account, nil
}
