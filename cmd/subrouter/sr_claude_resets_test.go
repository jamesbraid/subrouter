package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestClaudeLimitResetsSurviveRemoteStatusAndRender(t *testing.T) {
	var statuses []remoteServerUsageStatus
	if err := json.Unmarshal([]byte(`[{"id":"claude-test","provider":"claude","auth_mode":"oauth","limit_resets":{"known":true,"status":"live","eligible":true,"grants":[{"id":"fable","label":"Fable","remaining":2,"total":3,"usable_now":true,"requires_limit":true,"clears":["seven_day_overage_included"]},{"id":"full","label":"Full","remaining":1,"total":1,"usable_now":false,"requires_limit":true}]}}]`), &statuses); err != nil {
		t.Fatal(err)
	}
	rows := usageRowsFromServerUsageStatuses(statuses)
	if len(rows) != 1 || rows[0].limitResets == nil || len(rows[0].limitResets.Grants) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	cell := usageGridClaudeLimitResetCell(rows[0])
	for _, want := range []string{"2 Fable", "1 held Full"} {
		if !strings.Contains(cell.Text, want) {
			t.Fatalf("cell=%q missing%q", cell.Text, want)
		}
	}
	columns := claudeUsageGridColumns(rows, false, 200)
	found := false
	for _, column := range columns {
		found = found || column.Key == "LimitResets"
	}
	if !found {
		t.Fatal("Claude status omits reset column")
	}
	if got := usageGridValues(rows[0], "")["LimitResets"].Text; got != cell.Text {
		t.Fatalf("grid reset=%q, want%q", got, cell.Text)
	}
}

func TestClaudeLimitResetScopeIsUnknownRatherThanNoGrants(t *testing.T) {
	row := srUsageRow{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, limitResets: &accounts.LimitResetStatus{Status: "scope_required"}}
	if got := usageGridClaudeLimitResetCell(row).Text; got != "needs profile" {
		t.Fatalf("cell=%q", got)
	}
	row.limitResets = &accounts.LimitResetStatus{Status: "unavailable"}
	if got := usageGridClaudeLimitResetCell(row).Text; got != "unknown" {
		t.Fatalf("unknown cell=%q", got)
	}
	eligible := true
	row.limitResets = &accounts.LimitResetStatus{Known: true, Status: "live", Eligible: &eligible, Grants: []accounts.LimitResetGrant{{ID: "expired", Remaining: 3, ExpiresAt: "2000-01-01T00:00:00Z", UsableNow: true}}}
	if got := usageGridClaudeLimitResetCell(row).Text; got != "none" {
		t.Fatalf("expired cell=%q", got)
	}
}

func TestClaudeAPIKeyStatusOmitsSubscriptionResets(t *testing.T) {
	rows := []srUsageRow{{provider: accounts.ProviderClaude, authMode: accounts.AuthModeAPIKey}}
	for _, column := range claudeUsageGridColumns(rows, false, 200) {
		if column.Key == "LimitResets" {
			t.Fatal("API key status shows subscription resets")
		}
	}
}

func TestClaudeLimitResetLongLabelKeepsCountAndStateInNarrowTable(t *testing.T) {
	for _, tc := range []struct {
		name, state             string
		usable, paused, pending bool
	}{
		{name: "available", usable: true},
		{name: "held", state: " held"},
		{name: "paused", state: " paused", paused: true},
		{name: "pending", state: " pending", usable: true, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COLUMNS", "100")
			grant := accounts.LimitResetGrant{ID: "fable", Label: "New model launch: one Fable reset for Max, Team Premium and Enterprise Premium", Remaining: 1, Total: 1, UsableNow: tc.usable, Paused: tc.paused, ExpiresAt: time.Now().Add(48 * time.Hour).Format(time.RFC3339)}
			if tc.pending {
				grant.StartsAt = time.Now().Add(time.Hour).Format(time.RFC3339)
			}
			rows := []srUsageRow{{email: "claude3@example.test", provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth, limitResets: &accounts.LimitResetStatus{Known: true, Status: "live", Grants: []accounts.LimitResetGrant{grant}}}}
			var out bytes.Buffer
			displayUsageRows(&out, rows, false)
			var header, line string
			for _, row := range strings.Split(out.String(), "\n") {
				if strings.Contains(row, "Resets") {
					header = row
				}
				if strings.Contains(row, "claude3@example.test") {
					line = row
				}
			}
			start := strings.Index(header, "Resets")
			if start < 0 || start >= len(line) {
				t.Fatalf("missing reset column:\n%s", out.String())
			}
			width := 0
			for _, column := range usageGridColumnsForRows(&out, false, rows) {
				if column.Key == "LimitResets" {
					width = column.Width
				}
			}
			cell := strings.TrimSpace(line[start:min(start+width, len(line))])
			if !strings.HasPrefix(cell, "1"+tc.state+"/") {
				t.Fatalf("reset count/state/expiry hidden by long label: %q\n%s", cell, out.String())
			}
			t.Logf("%s: %s", tc.name, cell)
		})
	}
}
