package proxy

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAdmissionSpillHonorsAccountPolicyAfterEligibility(t *testing.T) {
	for _, tc := range []struct {
		name         string
		policies     map[string]accounts.AccountPolicy
		unavailable  string
		quotaBlocked string
		want         string
	}{
		{
			name: "disabled account is skipped",
			policies: map[string]accounts.AccountPolicy{
				"b@example.com": {Enabled: false},
			},
			want: "c@example.com",
		},
		{
			name: "highest eligible priority wins",
			policies: map[string]accounts.AccountPolicy{
				"c@example.com": {Enabled: true, Priority: 10},
			},
			want: "c@example.com",
		},
		{
			name: "exhausted priority tier falls through",
			policies: map[string]accounts.AccountPolicy{
				"c@example.com": {Enabled: true, Priority: 10},
			},
			quotaBlocked: "c@example.com",
			want:         "b@example.com",
		},
		{
			name: "full priority tier falls through",
			policies: map[string]accounts.AccountPolicy{
				"c@example.com": {Enabled: true, Priority: 10},
			},
			unavailable: "c@example.com",
			want:        "b@example.com",
		},
		{
			name: "all alternatives disabled",
			policies: map[string]accounts.AccountPolicy{
				"b@example.com": {Enabled: false},
				"c@example.com": {Enabled: false},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionSpillFixtureWith(t, []string{"a@example.com", "b@example.com", "c@example.com"}, nil)
			store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
			server := f.server
			server.AccountRef = NewAccountRef(store, server.Accounts, nil)
			server.Accounts = nil
			server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				return account, nil
			}
			for id, policy := range tc.policies {
				if err := server.AccountRef.policyStore.Update(accounts.ProviderClaude, id, policy); err != nil {
					t.Fatal(err)
				}
			}
			f.saturate("a@example.com", 1)
			if tc.unavailable != "" {
				f.saturate(tc.unavailable, 0)
			}
			if tc.quotaBlocked != "" {
				server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, tc.quotaBlocked, "claude-opus", time.Now().Add(time.Hour))
			}
			transport := usageLimitRetryTransport{
				server: &server, provider: accounts.ProviderClaude, poolModel: "claude-opus",
				attempt: &upstreamAttempt{getBody: func() (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("{}")), nil
				}},
			}
			got, lease, ok := transport.admissionSpill(t.Context(), "a@example.com", nil)
			if lease != nil {
				t.Cleanup(lease.release)
			}
			if got.ID != tc.want || ok != (tc.want != "") {
				t.Fatalf("spill account=%q ok=%v, want %q", got.ID, ok, tc.want)
			}
		})
	}
}
