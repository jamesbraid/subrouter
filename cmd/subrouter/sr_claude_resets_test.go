package main

import (
	"encoding/json"
	"strings"
	"testing"

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
	for _, want := range []string{"Fable 2", "Full 1 held"} {
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
