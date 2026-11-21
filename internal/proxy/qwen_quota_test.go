package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestQwenQuotaProtectedEnrollmentPublishesWeeklyUsage(t *testing.T) {
	store := qwensubscription.Store{Dir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", qwensubscription.CredentialInfo{AccessToken: "inference-access", RefreshToken: "inference-refresh", Subject: "owner", Email: "owner@example.test", MemberType: "plus", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var expired atomic.Bool
	var refreshes atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		headers := make(http.Header)
		switch r.URL.String() {
		case "https://auth.qwen.ai/api/v2/auths/refresh":
			refreshes.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("web session refresh received a bearer")
			}
			if _, err := r.Cookie("refresh_token"); err != nil {
				t.Error("session refresh omitted owned cookies")
			}
			if expired.Load() {
				body = `{"success":false,"httpCode":401,"errorCode":"NOT_LOGIN"}`
			} else {
				headers.Add("Set-Cookie", "refresh_token=rotated-cookie; Path=/; Secure; HttpOnly")
				body = `{"success":true,"data":{"access_token":"quota-secret","expires_in":900}}`
			}
		case "https://auth.qwen.ai/api/oauth/userinfo":
			if r.Header.Get("Authorization") != "Bearer inference-access" {
				t.Error("quota key reached inference userinfo")
			}
			body = `{"sub":"owner","email":"owner@example.test","member_type":"plus"}`
		default:
			if r.Header.Get("Authorization") != "Bearer quota-secret" {
				t.Error("inference grant reached web quota API")
			}
			if expired.Load() {
				body = `{"success":false,"httpCode":401,"errorCode":"NOT_LOGIN"}`
				break
			}
			if r.URL.Host == "auth.qwen.ai" && r.URL.Path == "/api/v2/auths/" {
				body = `{"success":true,"data":{"id":"web-owner","email":"owner@example.test"}}`
			} else if r.URL.Host == "sg-entitlement.qwen.com" && r.URL.Path == "/api/entitlement/agent/usage" {
				data, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"remainingPercent": 62.5, "resetAt": time.Now().Add(3 * 24 * time.Hour).UnixMilli()}})
				body = string(data)
			} else {
				t.Errorf("unexpected quota endpoint %s", r.URL.String())
				body = `{}`
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	ref, err := OpenAccountRefWithSources(t.Context(), accounts.CodexStore{Dir: t.TempDir()}, agentclaude.Store{Dir: t.TempDir()}, client, []OAuthAccountSource{store})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{AccountRef: ref, AdminToken: "admin", AccountImportToken: "import", SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}
	handler := server.Handler()
	request := func(payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/_subrouter/account-import", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer import")
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "100.64.0.2:3000"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	session := qwensubscription.QuotaSession{Version: 2, AccessToken: "quota-secret", ExpiresAt: time.Now().Add(15 * time.Minute), Cookies: []qwensubscription.QuotaCookie{{Name: "refresh_token", Value: "initial-cookie", Domain: "auth.qwen.ai", Path: "/", Secure: true, HTTPOnly: true}}, Subject: "forged", Email: "forged@example.test"}
	encoded, _ := json.Marshal(session)
	response := request(fmt.Sprintf(`{"provider":"qwen-oauth","qwen_subscription":{"label":"work","quota_session":%s}}`, encoded))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "quota-secret") {
		t.Fatalf("quota enrollment HTTP %d: %s", response.Code, response.Body.String())
	}
	response = request(fmt.Sprintf(`{"provider":"qwen-oauth","qwen_subscription":{"label":"work","quota_session":%s}}`, encoded))
	if response.Code != http.StatusOK || refreshes.Load() != 1 {
		t.Fatal("protected import retry rotated original grant", response.Code, refreshes.Load())
	}
	forged := strings.Replace(string(encoded), `"version":2`, `"version":2,"enrollment_fingerprint":"client-forged"`, 1)
	response = request(fmt.Sprintf(`{"provider":"qwen-oauth","qwen_subscription":{"label":"work","quota_session":%s}}`, forged))
	if response.Code != http.StatusBadRequest || refreshes.Load() != 1 {
		t.Fatal("client supplied internal enrollment fingerprint", response.Code)
	}
	statuses := ref.UsageStatusesFresh(t.Context())
	if len(statuses) != 1 || !statuses[0].AuthValid || !statuses[0].QuotaUsageKnown || len(statuses[0].Windows) != 1 || statuses[0].Windows[0].UsedPercent != 37.5 {
		t.Fatalf("weekly status was not published: %+v", statuses)
	}
	current, err := store.ListAccounts(t.Context())
	if err != nil || len(current) != 1 || current[0].CredentialIdentity() != acct.CredentialIdentity() {
		t.Fatal("quota enrollment rotated or replaced inference")
	}
	// Expired optional telemetry reports login-needed without disabling the
	// OAuth inference account. Do not reinterpret this as an auth failure.
	expired.Store(true)
	statuses = ref.UsageStatusesFresh(t.Context())
	if len(statuses) != 1 || !statuses[0].AuthValid || statuses[0].Error != "" || statuses[0].QuotaStatus != "login needed" {
		t.Fatalf("quota expiry damaged account health: %+v", statuses)
	}
	response = request(`{"provider":"qwen-oauth","qwen_subscription":{"label":"work","remove_quota":true}}`)
	if response.Code != http.StatusOK {
		t.Fatal("quota logout failed", response.Code)
	}

	response = request(fmt.Sprintf(`{"provider":"qwen-oauth","qwen_subscription":{"label":"work","quota_session":%s,"credential":{"access_token":"mixed"}}}`, encoded))
	if response.Code != http.StatusBadRequest {
		t.Fatal("mixed telemetry/inference mutation accepted")
	}
}
