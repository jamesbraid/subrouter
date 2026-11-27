package proxy

import (
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type claudeLimitResetEntry struct {
	status *accounts.LimitResetStatus
	at     time.Time
}

func (r *AccountRef) rememberClaudeLimitResets(account accounts.Account, status *accounts.LimitResetStatus) {
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
	r.claudeLimitResets[claudeSupplementalCacheKey(account)] = claudeLimitResetEntry{status: status, at: time.Now()}
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
		return &accounts.LimitResetStatus{Status: "unavailable"}
	}
	return entry.status
}
