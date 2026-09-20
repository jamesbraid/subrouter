package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestSRAccountPolicyCommandsUseSelectedServerAdminAPI(t *testing.T) {
	var requests []struct {
		method string
		path   string
		body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer admin-secret" {
			t.Errorf("Authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		request := struct {
			method string
			path   string
			body   map[string]any
		}{method: r.Method, path: r.URL.EscapedPath()}
		if r.Method == http.MethodPatch {
			if err := json.NewDecoder(r.Body).Decode(&request.body); err != nil {
				t.Errorf("decode request: %v", err)
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		enabled := request.body["enabled"]
		if enabled == nil {
			enabled = true
		}
		priority := request.body["priority"]
		if priority == nil {
			priority = float64(0)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "account/one", "provider": "claude", "enabled": enabled, "priority": priority,
		})
	}))
	defer server.Close()

	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: server.URL, AdminToken: "admin-secret"}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args string
		want string
	}{
		{args: "disable claude account/one", want: "Disabled claude account account/one (priority 0)."},
		{args: "enable claude account/one", want: "Enabled claude account account/one (priority 0)."},
		{args: "priority claude account/one -7", want: "Set claude account account/one priority to -7."},
		{args: "remove claude account/one", want: "Removed claude account account/one."},
	} {
		t.Run(tc.args, func(t *testing.T) {
			var output bytes.Buffer
			runner := srRunner{store: store, out: &output, errOut: &output, client: server.Client()}
			if err := runner.run(context.Background(), append([]string{"account"}, strings.Fields(tc.args)...)); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(output.String()); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}

	if len(requests) != 4 {
		t.Fatalf("requests = %d, want 4", len(requests))
	}
	for _, request := range requests {
		if request.path != "/_subrouter/accounts/claude/account%2Fone" {
			t.Fatalf("request path = %q", request.path)
		}
	}
	if requests[0].method != http.MethodPatch || requests[0].body["enabled"] != false {
		t.Fatalf("disable request = %#v", requests[0])
	}
	if requests[1].method != http.MethodPatch || requests[1].body["enabled"] != true {
		t.Fatalf("enable request = %#v", requests[1])
	}
	if requests[2].method != http.MethodPatch || requests[2].body["priority"] != float64(-7) {
		t.Fatalf("priority request = %#v", requests[2])
	}
	if requests[3].method != http.MethodDelete || requests[3].body != nil {
		t.Fatalf("remove request = %#v", requests[3])
	}
}

func TestSRAccountPolicyPriorityRejectsOutOfRangeWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: server.URL, AdminToken: "admin-secret"}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, priority := range []string{"1001", "-1001", "not-a-number"} {
		t.Run(priority, func(t *testing.T) {
			runner := srRunner{store: store, out: io.Discard, errOut: io.Discard, client: server.Client()}
			err := runner.run(t.Context(), []string{"account", "priority", "codex", "account", priority})
			if err == nil || !strings.Contains(err.Error(), "priority must be an integer from -1000 to 1000") {
				t.Fatalf("priority error = %v", err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid priority made %d requests", got)
	}
}

func TestSRAccountPolicyReportsServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "account not found", http.StatusNotFound)
	}))
	defer server.Close()

	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: server.URL, AdminToken: "admin-secret"}},
	}); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{store: store, out: io.Discard, errOut: io.Discard, client: server.Client()}
	err := runner.run(t.Context(), []string{"account", "disable", "codex", "missing"})
	if err == nil || !strings.Contains(err.Error(), "account not found") {
		t.Fatalf("server error = %v", err)
	}
}

func TestSRAccountListShowsSelectedServerPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer admin-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/_subrouter/accounts" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `[{"id":"account-1","provider":"codex","auth_mode":"oauth","enabled":false,"priority":17}]`)
	}))
	defer server.Close()

	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: server.URL, AdminToken: "admin-secret"}},
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	runner := srRunner{store: store, out: &output, errOut: &output, client: server.Client()}
	if err := runner.run(t.Context(), []string{"account", "list"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Server: team", "account-1", "disabled priority 17"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("account list did not contain %q:\n%s", want, output.String())
		}
	}
}

func TestSRHelpListsAccountPolicyCommands(t *testing.T) {
	var output bytes.Buffer
	runner := srRunner{out: &output}
	if err := runner.run(t.Context(), []string{"help"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sr account disable <provider> <account-id>",
		"sr account enable <provider> <account-id>",
		"sr account priority <provider> <account-id> <integer>",
		"sr account remove <provider> <account-id>",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("help did not contain %q", want)
		}
	}
}
