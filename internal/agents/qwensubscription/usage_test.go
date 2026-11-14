package qwensubscription

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quotaTestServer(t *testing.T, email string, remaining float64) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	expired := &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer web-secret" {
			t.Error("quota used the inference token or wrong credential")
		}
		if r.Header.Get("User-Agent") != "subrouter" {
			t.Error("quota request lacks client identification")
		}
		if expired.Load() {
			_, _ = io.WriteString(w, `{"success":false,"httpCode":401,"errorCode":"NOT_LOGIN","errorMsg":"web-secret"}`)
			return
		}
		if r.URL.Path == "/profile" {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"email": email}})
			return
		}
		if r.URL.Path != "/quota" {
			t.Error("unexpected telemetry endpoint")
		}
		if r.URL.Query().Get("biz_id") != "ai_qwen" {
			t.Error("missing vendor application selector")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "httpCode": 200, "data": map[string]any{"remainingPercent": remaining, "resetAt": time.Now().Add(3 * 24 * time.Hour).UnixMilli()}})
	}))
	t.Cleanup(server.Close)
	oldProfile, oldQuota := webProfileURL, quotaURL
	t.Cleanup(func() { webProfileURL, quotaURL = oldProfile, oldQuota })
	webProfileURL, quotaURL = server.URL+"/profile", server.URL+"/quota"
	return server, expired
}

func TestQwenQuotaHeadlessEnrollmentAndPolling(t *testing.T) {
	server, _ := quotaTestServer(t, "verified@example.test", 62.5)
	store := Store{Dir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", testCredential())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnrollQuota(t.Context(), server.Client(), "work", "Bearer web-secret"); err != nil {
		t.Fatal(err)
	}
	path, _ := store.quotaPath("work")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("quota credential not private")
	}
	// Reloading the store simulates the headless daemon reading its saved key;
	// no browser runtime or inference refresh grant is involved.
	restarted := Store{Dir: store.Dir}
	credential, _, err := restarted.read("work")
	if err != nil {
		t.Fatal(err)
	}
	windows, err := restarted.quotaWindows(t.Context(), server.Client(), "work", credential)
	if err != nil || len(windows) != 1 || windows[0].Name != "weekly" || windows[0].UsedPercent != 37.5 || windows[0].ResetAt.IsZero() {
		t.Fatal("weekly quota not preserved", windows, err)
	}
	listed, err := store.ListAccounts(t.Context())
	if err != nil || len(listed) != 1 || listed[0].Token != acct.Token || listed[0].CredentialIdentity() != acct.CredentialIdentity() {
		t.Fatal("quota setup changed inference ownership")
	}
	if err := store.RemoveQuota("work"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RefreshAccountIfNeeded(t.Context(), server.Client(), acct); err != nil {
		t.Fatal("quota logout broke inference", err)
	}
}

func TestQwenQuotaRejectsWrongAccountAndPreservesExistingKey(t *testing.T) {
	server, _ := quotaTestServer(t, "other@example.test", 50)
	store := Store{Dir: t.TempDir()}
	if _, err := store.SaveManagedCredential("work", testCredential()); err != nil {
		t.Fatal(err)
	}
	if err := store.EnrollQuota(t.Context(), server.Client(), "work", "web-secret"); err == nil {
		t.Fatal("different web identity was accepted")
	}
	path, _ := store.quotaPath("work")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("wrong-account authorization was persisted")
	}
}

func TestQwenQuotaExpiryDoesNotRotateOrInvalidateInference(t *testing.T) {
	server, expired := quotaTestServer(t, "verified@example.test", 10)
	store := Store{Dir: t.TempDir()}
	acct, err := store.SaveManagedCredential("work", testCredential())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnrollQuota(t.Context(), server.Client(), "work", "web-secret"); err != nil {
		t.Fatal(err)
	}
	expired.Store(true)
	credential, _, _ := store.read("work")
	_, err = store.quotaWindows(t.Context(), server.Client(), "work", credential)
	if !errors.Is(err, ErrQuotaLoginRequired) || strings.Contains(err.Error(), "web-secret") {
		t.Fatal("expiry misclassified or leaked a credential", err)
	}
	got, err := store.RefreshAccount(t.Context(), server.Client(), acct)
	if err != nil || got.Token != acct.Token {
		t.Fatal("telemetry expiry broke the inference account", err)
	}
	if _, ok, err := store.RemoveManagedAccount("work"); err != nil || !ok {
		t.Fatal(err)
	}
	path, _ := store.quotaPath("work")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("account removal retained its web key")
	}
}

func TestQwenQuotaNeverFollowsRedirects(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := webJSON(t.Context(), redirect.Client(), redirect.URL, "web-secret"); err == nil || strings.Contains(err.Error(), "web-secret") {
		t.Fatal("redirect not rejected", err)
	}
	if leaked.Load() != 0 {
		t.Fatal("web credential followed redirect")
	}
}

func TestQwenQuotaValidatesResponseUnitsAndCompleteness(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"missing percentage", `{"resetAt":1999999999999}`},
		{"missing reset", `{"remainingPercent":50}`},
		{"seconds instead of milliseconds", `{"remainingPercent":50,"resetAt":1999999999}`},
		{"invalid percentage", `{"remainingPercent":101,"resetAt":1999999999999}`},
		{"business auth error", `{"success":false,"httpCode":401,"errorCode":"NOT_LOGIN"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, test.body) }))
			defer server.Close()
			old := quotaURL
			quotaURL = server.URL
			t.Cleanup(func() { quotaURL = old })
			if _, err := fetchQuota(context.Background(), server.Client(), "web-secret", time.Now()); err == nil {
				t.Fatal("invalid quota accepted")
			}
		})
	}
}

func TestQwenQuotaDoesNotCrossEnrollmentIdentity(t *testing.T) {
	server, _ := quotaTestServer(t, "verified@example.test", 30)
	store := Store{Dir: t.TempDir()}
	credential := testCredential()
	if _, err := store.SaveManagedCredential("work", credential); err != nil {
		t.Fatal(err)
	}
	if err := store.EnrollQuota(t.Context(), server.Client(), "work", "web-secret"); err != nil {
		t.Fatal(err)
	}
	credential.Subject = "new-membership"
	if _, err := store.SaveManagedCredential("work", credential); err != nil {
		t.Fatal(err)
	}
	if _, err := store.quotaWindows(t.Context(), server.Client(), "work", credential); !errors.Is(err, ErrQuotaLoginRequired) {
		t.Fatal("quota key transferred to another enrollment", err)
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "quota" {
			t.Fatal("unexpected state directory", filepath.Join(store.Dir, entry.Name()))
		}
	}
}
