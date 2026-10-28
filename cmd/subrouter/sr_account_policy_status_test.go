package main

import (
	"encoding/json"
	"testing"
)

func TestRemoteUsageStatusDisabledAccountIsNotActive(t *testing.T) {
	var statuses []remoteServerUsageStatus
	if err := json.Unmarshal([]byte(`[{"id":"fresh","provider":"claude","auth_mode":"oauth","auth_valid":true,"active":true,"enabled":false,"windows":[{"Name":"5h"},{"Name":"7d"}]}]`), &statuses); err != nil {
		t.Fatal(err)
	}
	rows := usageRowsFromServerUsageStatuses(statuses)
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	row := rows[0]
	if row.active || displayRecommendedForNewSession(row) {
		t.Fatal("disabled account shown as usable")
	}
	if got := usageGridState(row); got != "disabled" {
		t.Fatalf("state %q", got)
	}
	if got := compactPickReason(row); got != "disabled" {
		t.Fatalf("reason %q", got)
	}
	if srStatusUsableState(srStatusAccountState(row)) {
		t.Fatal("disabled account counted as usable in JSON summary")
	}
	if got := srStatusAccountState(row); got != "disabled" {
		t.Fatalf("JSON state %q", got)
	}
}

func TestRemoteUsageStatusOlderServerDefaultsEnabled(t *testing.T) {
	var statuses []remoteServerUsageStatus
	if err := json.Unmarshal([]byte(`[{"id":"fresh","provider":"claude","auth_mode":"oauth","auth_valid":true,"active":true,"windows":[{"Name":"5h"},{"Name":"7d"}]}]`), &statuses); err != nil {
		t.Fatal(err)
	}
	rows := usageRowsFromServerUsageStatuses(statuses)
	if len(rows) != 1 || !rows[0].active || usageGridState(rows[0]) == "disabled" {
		t.Fatalf("older server account unexpectedly disabled: %+v", rows)
	}
}
