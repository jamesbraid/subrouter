package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

func TestQwenSubscriptionCommandsDispatchInTeamAndHostedModes(t *testing.T) {
	key, err := tenant.DeriveKey([]byte("0123456789abcdef0123456789abcdef"), "project", "team")
	if err != nil {
		t.Fatal(err)
	}
	var removals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/t/"+key)
		if path == r.URL.Path {
			t.Error("subscription command omitted tenant authorization")
		}
		switch path {
		case "/_subrouter/accounts":
			_, _ = io.WriteString(w, `[{"id":"qwen-subscription:work","label":"work","provider":"qwen-oauth","auth_mode":"oauth"}]`)
		case serverAccountImportPath:
			if r.Method == http.MethodGet {
				// Reject login before it can reach a live OAuth endpoint.
				_, _ = io.WriteString(w, `{"ok":true,"providers":["qwen"]}`)
				return
			}
			t.Error("unsupported subscription import reached mutation")
			removals.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for _, mode := range []broker.CredentialSource{broker.CredentialSourceTeam, broker.CredentialSourceHosted} {
		t.Run(string(mode), func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "cloud.json")
			t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
			t.Setenv("SUBROUTER_SERVER", "")
			t.Setenv("SUBROUTER_CODEX_SERVER", "")
			if err := broker.SaveConfig(configPath, broker.Config{Version: 1, AccessToken: "access", RefreshToken: "refresh", TeamID: "team", CredentialSource: mode, HostedURL: server.URL, TenantKey: key}); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			runner := srRunner{program: "sr", store: accounts.CodexStore{Dir: t.TempDir()}, client: server.Client(), out: &out, errOut: &out}
			if err := runner.run(t.Context(), []string{"qwen", "subscription", "list"}); err != nil || !strings.Contains(out.String(), "qwen-subscription:work") {
				t.Fatalf("subscription list was not dispatched: %v: %s", err, out.String())
			}
			for _, command := range []string{"login", "remove"} {
				if err := runner.run(t.Context(), []string{"qwen", "subscription", command, "work"}); err == nil || !strings.Contains(err.Error(), "does not advertise qwen-oauth") {
					t.Fatalf("subscription %s bypassed tenant preflight: %v", command, err)
				}
			}
		})
	}
	if removals.Load() != 0 {
		t.Fatal("rejected preflight mutated the hosted account pool")
	}
}

func TestQwenSubscriptionQuotaSetupUsesProtectedImportAndNeverPrintsKey(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != serverAccountImportPath || r.Header.Get("Authorization") != "Bearer import-secret" {
			t.Error("quota setup bypassed scoped import")
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"ok":true,"qwen_subscription_quota":true}`)
			return
		}
		var payload serverAccountImportRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.QwenSubscription == nil {
			t.Error("invalid quota payload")
			return
		}
		q := payload.QwenSubscription
		if q.Credential.AccessToken != "" || q.Credential.RefreshToken != "" || q.Remove {
			t.Error("quota setup changed inference authentication")
		}
		if calls.Add(1) == 1 {
			if q.QuotaToken == nil || *q.QuotaToken != "web-secret" || q.RemoveQuota {
				t.Error("quota login lost its key")
			}
		} else if !q.RemoveQuota || q.QuotaToken != nil {
			t.Error("quota logout sent credentials")
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	r := srRunner{client: server.Client(), in: strings.NewReader("web-secret\n"), out: &out, errOut: &out}
	target := srServerConfig{Name: "team", URL: server.URL, AccountImportToken: "import-secret"}
	for _, command := range []string{"quota-login", "quota-logout"} {
		if err := r.qwenRemote(t.Context(), target, []string{"subscription", command, "work"}); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 || strings.Contains(out.String(), "web-secret") {
		t.Fatal("quota setup leaked key or did not publish")
	}
}

func TestQwenSubscriptionQuotaSetupPreflightsBeforeReadingSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"providers":["qwen-oauth"]}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	r := srRunner{client: server.Client(), in: strings.NewReader("web-secret\n"), out: &out, errOut: &out}
	err := r.qwenRemote(t.Context(), srServerConfig{Name: "old", URL: server.URL, AccountImportToken: "import-secret"}, []string{"subscription", "quota-login", "work"})
	if err == nil || strings.Contains(out.String(), "Authorization") {
		t.Fatal("unsupported server prompted for a web credential", err)
	}
}

func TestQwenSubscriptionQuotaAppearsInCLIAndStatusJSON(t *testing.T) {
	reset := time.Now().Add(3 * 24 * time.Hour).UTC().Truncate(time.Second)
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{ID: "qwen-subscription:work", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, AuthChecked: true, AuthValid: true, PlanType: "plus", QuotaUsageKnown: true, Windows: []accounts.UsageWindow{{Name: "weekly", UsedPercent: 37.5, LimitWindowSeconds: 7 * 24 * 3600, ResetAt: reset, ResetAfterSeconds: 3 * 86400}}}})
	var out bytes.Buffer
	displayUsageRows(&out, rows, false)
	if !strings.Contains(out.String(), "7d") || !strings.Contains(out.String(), "62%/") {
		t.Fatalf("weekly quota missing from CLI:\n%s", out.String())
	}
	account := srStatusAccountFromRow(rows[0], time.Now())
	if account.WeeklyLeftPct == nil || *account.WeeklyLeftPct != 62.5 || account.WeeklyResetAt == nil {
		t.Fatal("weekly quota omitted from JSON", account)
	}
}

func TestQwenSubscriptionAuthErrorSuggestsSubscriptionLogin(t *testing.T) {
	row := srUsageRow{provider: accounts.ProviderQwenOAuth, email: "qwen-subscription:work account", err: errors.New("userinfo HTTP 401")}
	if got := usageRowErrorHint(row); got != " (re-add with: sr qwen subscription login 'work account')" {
		t.Fatalf("wrong Qwen recovery command: %s", got)
	}
}

func TestQwenSubscriptionCommandsUseSelectedServerAndProtectedImport(t *testing.T) {
	var removals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/accounts":
			if r.Header.Get("Authorization") != "Bearer admin-secret" {
				t.Error("inventory did not use admin authorization")
			}
			_, _ = io.WriteString(w, `[{"id":"qwen-subscription:work","label":"work","provider":"qwen-oauth","auth_mode":"oauth"},{"id":"qwen:plan","label":"plan","provider":"qwen","auth_mode":"apikey"}]`)
		case "/_subrouter/account-import":
			if r.Header.Get("Authorization") != "Bearer import-secret" {
				t.Error("enrollment did not use scoped import authorization")
			}
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"ok":true,"providers":["qwen-oauth"]}`)
				return
			}
			var payload serverAccountImportRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.QwenSubscription == nil || !payload.QwenSubscription.Remove || payload.QwenSubscription.Label != "work" || payload.Provider != "qwen-oauth" {
				t.Error("incorrect subscription removal payload")
			}
			if payload.QwenSubscription.Credential.AccessToken != "" || payload.QwenSubscription.Credential.RefreshToken != "" {
				t.Error("removal carried a credential")
			}
			removals.Add(1)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	r := srRunner{out: &out, errOut: &out, client: server.Client()}
	target := srServerConfig{Name: "team", URL: server.URL, AdminToken: "admin-secret", AccountImportToken: "import-secret"}
	if !isQwenManagementCommand([]string{"subscription", "login", "work"}) {
		t.Fatal("subscription command would launch the Token Plan client")
	}
	if err := r.qwenRemote(t.Context(), target, []string{"subscription", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "qwen-subscription:work") || strings.Contains(out.String(), "qwen:plan") {
		t.Fatal("subscription inventory mixed plan-key accounts")
	}
	if err := r.qwenRemote(t.Context(), target, []string{"subscription", "remove", "work"}); err != nil {
		t.Fatal(err)
	}
	if removals.Load() != 1 {
		t.Fatal("removal was not sent to the selected server")
	}
}

func TestQwenSubscriptionLoginPreflightsBeforeStartingOAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"providers":["qwen","qwen-token"]}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	r := srRunner{client: server.Client(), out: &out, errOut: &out}
	err := r.qwenRemote(t.Context(), srServerConfig{Name: "old", URL: server.URL, AccountImportToken: "import-secret"}, []string{"subscription", "login", "work"})
	if err == nil || !strings.Contains(err.Error(), "qwen-oauth") {
		t.Fatalf("unsupported server was not rejected before browser login: %v", err)
	}
	if strings.Contains(out.String(), "authorize") {
		t.Fatal("OAuth started before the remote capability check")
	}
}
