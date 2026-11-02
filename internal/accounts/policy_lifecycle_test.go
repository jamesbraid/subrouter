package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func TestAccountPolicyAvailableAt(t *testing.T) {
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		policy AccountPolicy
		want   bool
	}{
		{"unknown dates preserve enabled", AccountPolicy{Enabled: true}, true},
		{"disabled", AccountPolicy{Enabled: false, EndsAt: now.Add(time.Hour)}, false},
		{"future end", AccountPolicy{Enabled: true, EndsAt: now.Add(time.Nanosecond)}, true},
		{"end reached", AccountPolicy{Enabled: true, EndsAt: now}, false},
		{"end passed", AccountPolicy{Enabled: true, EndsAt: now.Add(-time.Nanosecond)}, false},
		{"past renewal is informational", AccountPolicy{Enabled: true, RenewsAt: now.Add(-time.Hour)}, true},
		{"future renewal is informational", AccountPolicy{Enabled: true, RenewsAt: now.Add(time.Hour)}, true},
		{"renewal cannot override end", AccountPolicy{Enabled: true, EndsAt: now, RenewsAt: now.Add(time.Hour)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.AvailableAt(now); got != tt.want {
				t.Fatalf("AvailableAt = %v; want %v", got, tt.want)
			}
		})
	}
	input := []AccountWithPolicy{
		{Account: account.Account{ID: "first"}, Policy: AccountPolicy{Enabled: true}},
		{Account: account.Account{ID: "expired"}, Policy: AccountPolicy{Enabled: true, EndsAt: now}},
		{Account: account.Account{ID: "disabled"}, Policy: AccountPolicy{Enabled: false}},
		{Account: account.Account{ID: "last"}, Policy: AccountPolicy{Enabled: true, RenewsAt: now.Add(-time.Hour)}},
	}
	got := FilterAvailable(input, now)
	if len(got) != 2 || got[0].Account.ID != "first" || got[1].Account.ID != "last" {
		t.Fatalf("available accounts = %+v", got)
	}
	if len(input) != 4 || input[1].Account.ID != "expired" {
		t.Fatal("filter mutated input")
	}
}

func TestPolicyStoreLifecycleRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	store, err := NewPolicyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := store.Load(account.ProviderClaude, "new")
	if err != nil || !unknown.Enabled || !unknown.EndsAt.IsZero() || !unknown.RenewsAt.IsZero() {
		t.Fatalf("default policy = %+v, %v", unknown, err)
	}
	zone := time.FixedZone("PST", -8*60*60)
	end := time.Date(2026, 11, 1, 8, 30, 0, 0, zone)
	renewal := end.Add(-24 * time.Hour)
	policy := AccountPolicy{Enabled: true, Priority: 3, EndsAt: end, RenewsAt: renewal}
	if err := store.Update(account.ProviderClaude, "ending", policy); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(account.ProviderClaude, "ending")
	if err != nil {
		t.Fatal(err)
	}
	if !got.EndsAt.Equal(end) || !got.RenewsAt.Equal(renewal) || got.EndsAt.Location() != time.UTC || got.RenewsAt.Location() != time.UTC {
		t.Fatalf("dates did not round-trip in UTC: %+v", got)
	}
	if got.AvailableAt(end) {
		t.Fatal("past end was not retained")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"ends_at": "2026-11-01T16:30:00Z"`) || !strings.Contains(string(body), `"renews_at": "2026-10-31T16:30:00Z"`) {
		t.Fatalf("stored dates are not normalized: %s", body)
	}
	if err := store.Update(account.ProviderClaude, "ending", AccountPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "ends_at") || strings.Contains(string(body), "renews_at") {
		t.Fatalf("zero dates should be omitted: %s", body)
	}
	got, err = store.Load(account.ProviderClaude, "ending")
	if err != nil || !got.EndsAt.IsZero() || !got.RenewsAt.IsZero() {
		t.Fatalf("cleared dates = %+v, %v", got, err)
	}
}

func TestAccountPolicyLifecycleJSONCompatibility(t *testing.T) {
	var legacy AccountPolicy
	if err := json.Unmarshal([]byte(`{"enabled":true,"priority":5}`), &legacy); err != nil || !legacy.EndsAt.IsZero() || !legacy.RenewsAt.IsZero() {
		t.Fatalf("legacy policy = %+v, %v", legacy, err)
	}
	for _, field := range []string{"ends_at", "renews_at"} {
		path := filepath.Join(t.TempDir(), "policy.json")
		body := `{"policies":[{"provider":"claude","account_id":"bad","policy":{"enabled":true,"` + field + `":"tomorrow"}}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPolicyStore(path); err == nil {
			t.Fatalf("malformed %s date accepted", field)
		}
	}
}

func TestFilterEligibleExcludesEndedPriority(t *testing.T) {
	now := time.Now()
	input := []AccountWithPolicy{
		{Account: account.Account{ID: "ended"}, Policy: AccountPolicy{Enabled: true, Priority: 10, EndsAt: now.Add(-time.Hour)}},
		{Account: account.Account{ID: "renewed"}, Policy: AccountPolicy{Enabled: true, Priority: 3, RenewsAt: now.Add(-time.Hour)}},
		{Account: account.Account{ID: "ordinary"}, Policy: AccountPolicy{Enabled: true, Priority: 3}},
		{Account: account.Account{ID: "lower"}, Policy: AccountPolicy{Enabled: true, Priority: 1}},
	}
	got := FilterEligible(input)
	if len(got) != 2 || got[0].Account.ID != "renewed" || got[1].Account.ID != "ordinary" {
		t.Fatalf("highest available priority accounts = %+v", got)
	}
}
