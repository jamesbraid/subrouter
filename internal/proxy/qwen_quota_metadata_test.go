package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
)

func TestQwenQuotaMetadataUsesImportPermissionAndSelectedStore(t *testing.T) {
	store := qwensubscription.Store{Dir: t.TempDir()}
	credential := qwensubscription.CredentialInfo{AccessToken: "inference-secret", RefreshToken: "refresh-secret", Subject: "owner", Email: "verified@example.test", MemberType: "pro", ExpiresAt: time.Now().Add(time.Hour)}
	for _, label := range []string{"work", "other", "pending"} {
		copy := credential
		copy.Subject = label
		copy.RefreshToken += "-" + label
		if label == "other" {
			copy.Email = "other@example.test"
		}
		if label == "pending" {
			copy.VerificationPending = true
		}
		if _, err := store.SaveManagedCredential(label, copy); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, nil, []OAuthAccountSource{store})
	if err != nil {
		t.Fatal(err)
	}
	handler := (Server{AccountRef: ref, AdminToken: "admin-secret", AccountImportToken: "import-secret"}).Handler()
	request := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "100.64.0.2:3000"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	response := request("/_subrouter/account-import?account_id=qwen-subscription%3Awork", "import-secret")
	var got struct {
		OK      bool                       `json:"ok"`
		Account struct{ ID, Email string } `json:"qwen_subscription_account"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || response.Code != 200 || !got.OK || got.Account.ID != "qwen-subscription:work" || got.Account.Email != "verified@example.test" {
		t.Fatalf("metadata HTTP%d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"inference-secret", "refresh-secret", "other@example.test"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("metadata exposed unselected or secret data")
		}
	}
	for _, test := range []struct {
		query string
		code  int
	}{{"account_id=qwen-subscription:missing", 404}, {"account_id=qwen-subscription:pending", 404}, {"account_id=codex:work", 400}, {"account_id=", 400}, {"account_id=qwen-subscription:work&account_id=qwen-subscription:other", 400}} {
		if result := request("/_subrouter/account-import?"+test.query, "import-secret"); result.Code != test.code {
			t.Fatalf("%s: HTTP%d", test.query, result.Code)
		}
	}
	if result := request("/_subrouter/account-import?account_id=qwen-subscription:work", ""); result.Code == 200 || strings.Contains(result.Body.String(), credential.Email) {
		t.Fatal("metadata bypassed authentication")
	}
	if result := request("/_subrouter/account-import", "import-secret"); result.Code != 200 || strings.Contains(result.Body.String(), "qwen_subscription_account") {
		t.Fatal("unselected capabilities changed")
	}
	// A separate AccountRef cannot resolve an account from the first store.
	empty := qwensubscription.Store{Dir: t.TempDir()}
	other, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, nil, []OAuthAccountSource{empty})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/_subrouter/account-import?account_id=qwen-subscription:work", nil)
	req.RemoteAddr = "100.64.0.2:3000"
	req.Header.Set("Authorization", "Bearer import-secret")
	out := httptest.NewRecorder()
	(Server{AccountRef: other, AccountImportToken: "import-secret"}).Handler().ServeHTTP(out, req)
	if out.Code != 404 {
		t.Fatalf("other store resolved account: HTTP%d", out.Code)
	}
}
