package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
)

func TestQwenQuotaBrowserCookieCaptureUsesReadOnlySQLite(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 unavailable")
	}
	database := filepath.Join(t.TempDir(), "cookies.sqlite")
	fixture := `CREATE TABLE moz_cookies(name,value,host,path,expiry,isSecure,isHttpOnly,originAttributes);
INSERT INTO moz_cookies VALUES('refresh_token','fixture-only','.qwen.ai','/',1795192810107,1,1,'');
INSERT INTO moz_cookies VALUES('analytics','must-not-import','.qwen.ai','/',1795192810107,0,0,'');`
	if body, err := exec.Command(sqlite, database, fixture).CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, body)
	}
	body, err := qwenCookieQuery(t.Context(), sqlite, database, "SELECT name,value,host,path,expiry,isSecure,isHttpOnly FROM moz_cookies WHERE name='refresh_token'")
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := decodeQwenRefreshCookie(body, time.Unix(1795106410, 0))
	if err != nil || cookie.Expires.Unix() != 1795192810 || cookie.Name != "refresh_token" || !cookie.HTTPOnly || !cookie.Secure {
		t.Fatalf("private refresh cookie metadata incorrect: %v", err)
	}
	if strings.Contains(string(body), "must-not-import") {
		t.Fatal("query read unrelated cookie material")
	}
	if _, err := qwenCookieQuery(t.Context(), sqlite, database, "DELETE FROM moz_cookies"); err == nil {
		t.Fatal("cookie database was writable")
	}
}

func TestQwenQuotaBrowserRejectsUnusableCookies(t *testing.T) {
	const valid = `{"name":"refresh_token","value":"fixture-only","host":".qwen.ai","path":"/","expiry":1795192810,"isSecure":1,"isHttpOnly":1}`
	for _, body := range []string{
		`[]`,
		`[` + valid + `,` + valid + `]`,
		`[` + strings.ReplaceAll(valid, ".qwen.ai", ".example.com") + `]`,
		`[` + strings.ReplaceAll(valid, ".qwen.ai", "qwen.ai") + `]`,
		`[` + strings.ReplaceAll(valid, `"isSecure":1`, `"isSecure":0`) + `]`,
		`[` + strings.ReplaceAll(valid, `"isHttpOnly":1`, `"isHttpOnly":0`) + `]`,
		`[` + strings.ReplaceAll(valid, "1795192810", "1") + `]`,
	} {
		if _, err := decodeQwenRefreshCookie([]byte(body), time.Unix(1795106410, 0)); err == nil {
			t.Fatal("unusable or ambiguous cookie accepted")
		}
	}
}

func TestQwenSubscriptionQuotaBrowserUsesEnrolledIdentityAndCookieOnlyImport(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	var posts, authorizations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == serverAccountImportPath && req.Method == http.MethodGet:
			result := map[string]any{"ok": true, "qwen_subscription_quota": true, "qwen_subscription_quota_version": 2}
			if req.URL.Query().Get("account_id") == "qwen-subscription:work" {
				result["qwen_subscription_account"] = map[string]string{"id": "qwen-subscription:work", "email": "verified@example.test"}
			}
			_ = json.NewEncoder(w).Encode(result)
		case req.URL.Path == serverAccountImportPath && req.Method == http.MethodPost:
			var payload serverAccountImportRequest
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.QwenSubscription == nil || payload.QwenSubscription.QuotaSession == nil {
				t.Fatal("quota session missing")
			}
			grant := payload.QwenSubscription.QuotaSession
			if grant.AccessToken != "" || len(grant.Cookies) != 1 || payload.QwenSubscription.Credential.AccessToken != "" {
				t.Fatal("browser enrollment sent an inference credential or a client-refreshed token")
			}
			posts++
			if posts == 1 {
				// Simulate an import whose successful acknowledgement was lost.
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	runner := srRunner{client: server.Client(), out: &out, errOut: &out, qwenQuotaAuthorize: func(_ context.Context, _ io.Writer, email string) (qwensubscription.QuotaSession, error) {
		authorizations++
		if email != "verified@example.test" {
			t.Fatal("browser login used caller-supplied identity")
		}
		return qwensubscription.QuotaSession{Version: 2, Cookies: []qwensubscription.QuotaCookie{{Name: "refresh_token", Value: "fixture-secret", Domain: ".qwen.ai", Path: "/", Secure: true, HTTPOnly: true}}}, nil
	}}
	config := srServerConfig{Name: "selected", URL: server.URL, AccountImportToken: "import-secret"}
	if err := runner.qwenRemote(t.Context(), config, []string{"subscription", "quota-login", "work"}); err == nil {
		t.Fatal("missing import acknowledgement was accepted")
	}
	if _, pending, err := readQwenQuotaPending(runner.qwenQuotaPendingPath(&config, "work"), "verified@example.test"); err != nil || !pending {
		t.Fatal("retry credential was not saved privately")
	}
	if err := runner.qwenRemote(t.Context(), config, []string{"subscription", "quota-login", "work"}); err != nil {
		t.Fatal(err)
	}
	if posts != 2 || authorizations != 1 || strings.Contains(out.String(), "fixture-secret") {
		t.Fatal("enrollment did not finish or exposed the cookie")
	}
	if _, pending, err := readQwenQuotaPending(runner.qwenQuotaPendingPath(&config, "work"), "verified@example.test"); err != nil || pending {
		t.Fatal("acknowledged enrollment retained the client credential")
	}
	stale := qwensubscription.QuotaSession{Version: 2, Cookies: []qwensubscription.QuotaCookie{{Name: "refresh_token", Value: "revoked-fixture"}}}
	if err := saveQwenQuotaPending(runner.qwenQuotaPendingPath(&config, "work"), "verified@example.test", stale); err != nil {
		t.Fatal(err)
	}
	if err := runner.qwenRemote(t.Context(), config, []string{"subscription", "quota-login", "--fresh", "work"}); err != nil {
		t.Fatal(err)
	}
	if authorizations != 2 || posts != 3 {
		t.Fatal("fresh enrollment did not replace the rejected pending credential")
	}
}
