package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestOpenAIAPIKeyCreditFailureFailsOverAndCommitsServingAccount(t *testing.T) {
	const primary = "apikey:spent"
	const secondary = "apikey:funded"
	const creditError = `{"error":{"code":"credit_balance_exhausted","message":"You have no credits remaining.","type":"insufficient_quota"}}`
	for _, tc := range []struct {
		name, failure string
		wantExhausted bool
	}{
		{"exhausted credit", creditError, true},
		{"legacy quota code", `{"error":{"code":"insufficient_quota"}}`, true},
		{"transient rate limit", `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var attempts []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts = append(attempts, r.Header.Get("Authorization"))
				mu.Unlock()
				if r.URL.Path != "/v1/responses" {
					t.Errorf("path=%s", r.URL.Path)
				}
				if r.Header.Get("ChatGPT-Account-ID") != "" || r.Header.Get("X-Subrouter-Account-ID") != "" {
					t.Error("routing-only header leaked upstream")
				}
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `"input":"Reply OK."`) {
					t.Errorf("request body lost during replay: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") == "Bearer spent-key" {
					w.WriteHeader(429)
					io.WriteString(w, tc.failure)
					return
				}
				if r.Header.Get("Authorization") != "Bearer funded-key" {
					t.Error("unexpected upstream credential")
					w.WriteHeader(401)
					return
				}
				io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","output":[]}`)
			}))
			defer upstream.Close()
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put("codex", "sticky", primary, ""); err != nil {
				t.Fatal(err)
			}
			scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			server := Server{Accounts: []accounts.Account{
				{ID: primary, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Token: "spent-key"},
				{ID: secondary, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Token: "funded-key"},
			}, Sessions: store, SchedulerRef: scheduler, APIUpstream: mustParseURL(t, upstream.URL), MaxBodyBytes: 1024}
			handler := server.Handler()
			post := func(sid string) {
				req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-6.2-sol","input":"Reply OK.","max_output_tokens":16}`))
				req.Header.Set("X-Subrouter-Session", sid)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
			}
			post("sticky")
			mu.Lock()
			got := append([]string(nil), attempts...)
			mu.Unlock()
			if want := []string{"Bearer spent-key", "Bearer funded-key"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("attempts=%v want=%v", got, want)
			}
			assignment, ok := store.Get("codex", "sticky")
			if !ok || assignment.AccountID != secondary {
				t.Fatalf("sticky assignment=%+v", assignment)
			}
			if got := scheduler.Get().Exhausted(accounts.ProviderCodex, primary); got != tc.wantExhausted {
				t.Fatalf("exhausted=%v want=%v", got, tc.wantExhausted)
			}
			post("sticky")
			if tc.wantExhausted {
				post("new-session")
			}
			mu.Lock()
			defer mu.Unlock()
			for _, key := range attempts[2:] {
				if key != "Bearer funded-key" {
					t.Errorf("follow-up routed to exhausted credential: %v", attempts)
				}
			}
		})
	}
}

func TestOpenAIAPIKeyCreditFailureRespectsForcedPinAndRequestBudget(t *testing.T) {
	const creditError = `{"error":{"code":"credit_balance_exhausted","type":"insufficient_quota","message":"You have no credits remaining."}}`
	for _, tc := range []struct {
		name                string
		forced              bool
		count, wantAttempts int
	}{
		{"explicit pin does not change account", true, 2, 1},
		{"all credits spent walks keys once", false, 3, 3},
		{"large pool retains shared attempt budget", false, 10, replayablePostMaxAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var attempts []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts = append(attempts, r.Header.Get("Authorization"))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(429)
				io.WriteString(w, creditError)
			}))
			defer upstream.Close()
			var pool []accounts.Account
			for i := 0; i < tc.count; i++ {
				pool = append(pool, accounts.Account{ID: fmt.Sprintf("apikey:key-%d", i), Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Token: fmt.Sprintf("test-key-%d", i)})
			}
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put("codex", "session", pool[0].ID, ""); err != nil {
				t.Fatal(err)
			}
			scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			server := Server{Accounts: pool, Sessions: store, SchedulerRef: scheduler, APIUpstream: mustParseURL(t, upstream.URL), MaxBodyBytes: 1024}
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-6.2-sol","input":"Reply OK."}`))
			req.Header.Set("X-Subrouter-Session", "session")
			if tc.forced {
				req.Header.Set("X-Subrouter-Account-ID", pool[0].ID)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != 429 || rec.Body.String() != creditError {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			mu.Lock()
			got := append([]string(nil), attempts...)
			mu.Unlock()
			if len(got) != tc.wantAttempts {
				t.Fatalf("attempts=%v count=%d want=%d", got, len(got), tc.wantAttempts)
			}
			seen := map[string]bool{}
			for _, key := range got {
				if seen[key] {
					t.Fatalf("exhausted credential retried: %v", got)
				}
				seen[key] = true
			}
			if !scheduler.Get().Exhausted(accounts.ProviderCodex, pool[0].ID) {
				t.Fatal("credit refusal did not exclude spent key")
			}
			assignment, _ := store.Get("codex", "session")
			if assignment.AccountID != pool[0].ID {
				t.Fatalf("failed request changed sticky assignment: %+v", assignment)
			}
		})
	}
}

func TestCodexCreditFailureClassification(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"credit_balance_exhausted","type":"insufficient_quota"}}`,
		`{"error":{"type":"insufficient_quota"}}`,
		`{"type":"response.failed","response":{"error":{"code":"credit_balance_exhausted"}}}`,
	} {
		if got := codexTurnFailureClass([]byte(body)); got != codexFailureQuota {
			t.Errorf("class=%v for %s", got, body)
		}
	}
}
