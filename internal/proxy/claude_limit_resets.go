package proxy

import (
	"context"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type claudeLimitResetEntry struct {
	status         *accounts.LimitResetStatus
	at             time.Time
	requestStarted time.Time
}

func (r *AccountRef) rememberClaudeLimitResets(account accounts.Account, status *accounts.LimitResetStatus) {
	r.rememberClaudeLimitResetsAfter(account, status, time.Now())
}

func (r *AccountRef) rememberClaudeLimitResetsAfter(account accounts.Account, status *accounts.LimitResetStatus, started time.Time) {
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	if r.claudeLimitResets == nil {
		r.claudeLimitResets = map[string]claudeLimitResetEntry{}
	}
	for key, entry := range r.claudeLimitResets {
		if time.Since(entry.at) >= usageWindowsLastGoodTTL {
			delete(r.claudeLimitResets, key)
		}
	}
	key := claudeSupplementalCacheKey(account)
	previous := r.claudeLimitResets[key]
	orderAt := previous.requestStarted
	if orderAt.IsZero() {
		orderAt = previous.at
	}
	if !started.After(orderAt) {
		return
	}
	r.claudeLimitResets[key] = claudeLimitResetEntry{status: status, at: time.Now(), requestStarted: started}
}

func (r *AccountRef) claudeLimitResetStatus(account accounts.Account) *accounts.LimitResetStatus {
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	key := claudeSupplementalCacheKey(account)
	entry, ok := r.claudeLimitResets[key]
	if !ok || time.Since(entry.at) >= usageWindowsTTL {
		if _, denied := r.claudeUsageScopeDenied[key]; denied {
			return &accounts.LimitResetStatus{Status: "scope_required"}
		}
		if failure, throttled := r.usageWindowsFailures[usageWindowsFailureKey(account.ID+"\x00"+string(account.Provider))]; throttled && time.Now().Before(failure.retryAt) {
			return &accounts.LimitResetStatus{Status: "throttled"}
		}
		return &accounts.LimitResetStatus{Status: "unavailable"}
	}
	return entry.status
}

// Passive inference headers do not contain reset grants. Their timestamps must
// not postpone the existing usage endpoint read that refreshes those grants.
func (r *AccountRef) claudeLimitResetsNeedRefresh(ctx context.Context, account accounts.Account) bool {
	if account.Provider != accounts.ProviderClaude || account.AuthMode != accounts.AuthModeOAuth || r.claudeUsageScopeDeniedFor(ctx, account) {
		return false
	}
	key := claudeSupplementalCacheKey(account)
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	if failure, throttled := r.usageWindowsFailures[usageWindowsFailureKey(account.ID+"\x00"+string(account.Provider))]; throttled && time.Now().Before(failure.retryAt) {
		return false
	}
	entry, seen := r.claudeLimitResets[key]
	_, scopeChecked := r.claudeUsageScopeChecked[key]
	return (scopeChecked || seen) && (!seen || time.Since(entry.at) >= usageWindowsTTL)
}

func (r *AccountRef) claudeStatusResetsNeedRefresh(ctx context.Context, rows []AccountUsageStatus) bool {
	for _, row := range rows {
		if row.Provider == accounts.ProviderClaude && row.AuthMode == accounts.AuthModeOAuth && r.claudeLimitResetsNeedRefresh(ctx, r.credentialSnapshot(row.Provider, row.ID)) {
			return true
		}
	}
	return false
}
