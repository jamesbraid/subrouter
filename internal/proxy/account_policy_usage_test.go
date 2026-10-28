package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestUsageStatusShowsDisabledPolicyOnCachedQuota(t *testing.T) {
	account := policyRoutingAccount("fresh")
	account.Provider = accounts.ProviderClaude
	server, policies := accountPolicyRoutingServer(t, account)
	server.AccountRef.usageStatusCache = []AccountUsageStatus{{AccountStatus: AccountStatus{ID: account.ID, Provider: account.Provider, AuthValid: true}, Active: true, Windows: []accounts.UsageWindow{{Name: "5h"}, {Name: "7d"}}}}
	server.AccountRef.usageStatusAt = time.Now()
	handler := server.Handler()
	for _, enabled := range []bool{false, true} {
		if err := policies.Update(account.Provider, account.ID, accounts.AccountPolicy{Enabled: enabled}); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status?snapshot=1", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status %d: %s", response.Code, response.Body.String())
		}
		var rows []map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0]["enabled"] != enabled || (rows[0]["active"] == true) != enabled {
			t.Fatalf("enabled %v: %+v", enabled, rows)
		}
	}
	if !server.AccountRef.usageStatusCache[0].Active {
		t.Fatal("response decoration changed shared quota cache")
	}
}
