package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestSRSubscriptionDateCommands(t *testing.T) {
	for _, tc := range []struct {
		command string
		value   string
		want    any
	}{
		{"ends-at", "2026-11-08T00:00:00-08:00", "2026-11-08T08:00:00Z"},
		{"renews-at", "2026-12-01T00:00:00+02:00", "2026-11-30T22:00:00Z"},
		{"ends-at", "clear", nil},
		{"renews-at", "clear", nil},
	} {
		t.Run(tc.command+"/"+tc.value, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPatch || r.URL.EscapedPath() != "/_subrouter/accounts/claude/account%2Fone" || r.Header.Get("Authorization") != "Bearer test-admin" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				field := strings.ReplaceAll(tc.command, "-", "_")
				got, present := body[field]
				if len(body) != 1 || !present || got != tc.want {
					t.Errorf("patch = %#v; want %s=%#v", body, field, tc.want)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"account/one","provider":"claude","enabled":true,"priority":7}`))
			}))
			defer server.Close()
			store := accounts.CodexStore{Dir: t.TempDir()}
			if err := defaultSRServerStore(store).save(srServerFile{Default: "team", Servers: []srServerConfig{{Name: "team", URL: server.URL, AdminToken: "test-admin"}}}); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			runner := srRunner{store: store, out: &out, errOut: &out, client: server.Client()}
			if err := runner.run(context.Background(), []string{"account", tc.command, "claude", "account/one", tc.value}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("requests=%d; want 1", calls)
			}
		})
	}
}

func TestSRSubscriptionDateRejectsAmbiguousDates(t *testing.T) {
	for _, date := range []string{"tomorrow", "2026-11-08", "2026-11-08T00:00:00", "0001-01-01T00:00:00Z"} {
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out}
		err := runner.accountPolicyCommand(context.Background(), srServerConfig{}, []string{"ends-at", "claude", "account", date})
		if err == nil || !strings.Contains(err.Error(), "RFC3339") {
			t.Fatalf("date=%q error=%v", date, err)
		}
	}
}

func TestSRSubscriptionLifecycleDisplay(t *testing.T) {
	now := time.Now().UTC()
	end := now.Add(-time.Minute).Truncate(time.Second)
	renew := now.Add(-time.Hour).Truncate(time.Second)
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{
		{ID: "ended", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, EndsAt: end, Active: true},
		{ID: "renewing", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, RenewsAt: renew},
	})
	byID := make(map[string]srUsageRow)
	for _, row := range rows {
		byID[row.accountID] = row
	}
	ended := byID["ended"]
	if !ended.ended || ended.disabled || usageGridState(ended) != "expired" || displayRecommendedForNewSession(ended) {
		t.Fatalf("ended account should be expired, retain configured enablement, and be unavailable: %+v", ended)
	}
	if byID["renewing"].ended {
		t.Fatal("a past expected renewal must not expire the account")
	}
	var out bytes.Buffer
	displayUsageRowsPerGroup(&out, rows)
	for _, want := range []string{"expired", "subscription ended", "subscription ends " + end.Format(time.RFC3339), "expected renewal " + renew.Format(time.RFC3339) + " (informational)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q: %s", want, out.String())
		}
	}
	status := buildSRStatusJSON(rows, "team", now)
	claude := status.Providers["claude"]
	accountsByID := make(map[string]srStatusAccountJSON)
	for _, account := range claude.Accounts {
		accountsByID[account.ID] = account
	}
	if claude.Summary.Usable != 1 || accountsByID["ended"].State != "expired" || !accountsByID["ended"].EndsAt.Equal(end) || !accountsByID["renewing"].RenewsAt.Equal(renew) {
		t.Fatalf("JSON lifecycle=%+v", claude)
	}
}

func TestSRSubscriptionExpiryUsesServingClock(t *testing.T) {
	now := time.Now()
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		end       time.Time
		available *bool
		want      bool
	}{
		{"server already ended", now.Add(time.Hour), &no, true},
		{"server still available", now.Add(-time.Hour), &yes, false},
		{"older server elapsed", now.Add(-time.Hour), nil, true},
		{"older server pending", now.Add(time.Hour), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{ID: "account", Provider: accounts.ProviderClaude, EndsAt: tc.end, Enabled: &yes, Available: tc.available}})
			if len(rows) != 1 || rows[0].ended != tc.want {
				t.Fatalf("rows=%+v want ended=%v", rows, tc.want)
			}
		})
	}
}
