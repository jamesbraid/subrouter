package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestClaudeModelQuotaProbeRespectsMeasuredExhaustion(t *testing.T) {
	for _, pool := range []string{"claudeopus", "claudesonnet"} {
		for _, tc := range []struct {
			name             string
			modelFresh       bool
			modelReset       bool
			accountExhausted bool
			accountReset     bool
			wantProbe        bool
		}{
			{name: "stale model without reset", wantProbe: true},
			{name: "fresh model exhaustion", modelFresh: true},
			{name: "stale model waiting for reset", modelReset: true},
			{name: "fresh model waiting for reset", modelFresh: true, modelReset: true},
			{name: "stale account without reset", accountExhausted: true, wantProbe: true},
			{name: "stale account waiting for reset", accountExhausted: true, accountReset: true},
		} {
			t.Run(pool+"/"+tc.name, func(t *testing.T) {
				modelScore := selectacct.Score{
					AccountID: "candidate", Provider: accounts.ProviderClaude,
					Fresh: tc.modelFresh,
				}
				if tc.modelReset {
					modelScore.ExhaustedResetAt = time.Now().Add(time.Hour)
				}
				score := selectacct.Score{
					AccountID: "candidate", Provider: accounts.ProviderClaude,
					Headroom: 1, ShortHeadroom: 1,
					ModelScores: map[string]selectacct.Score{pool: modelScore},
				}
				if tc.accountExhausted {
					score.Headroom, score.ShortHeadroom = 0, 0
				}
				if tc.accountReset {
					score.ExhaustedResetAt = time.Now().Add(time.Hour)
				}
				base := selectacct.NewScheduler([]selectacct.Score{score})
				server := Server{SchedulerRef: selectacct.NewSchedulerRef(base)}
				policies := []accounts.AccountWithPolicy{{
					Account: accounts.Account{ID: "candidate", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
					Policy:  accounts.AccountPolicy{Enabled: true},
				}}
				account, ok := server.pickClaudeModelQuotaProbe(pool, base, policies)
				if ok != tc.wantProbe {
					t.Fatalf("probe account = %q, available = %t, want available = %t", account.ID, ok, tc.wantProbe)
				}
				if ok && account.ID != "candidate" {
					t.Fatalf("probe account = %q, want candidate", account.ID)
				}
			})
		}
	}
}
