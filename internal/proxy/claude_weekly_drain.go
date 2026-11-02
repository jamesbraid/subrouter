package proxy

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// claimWeeklyDrain keeps a fan-out from rewriting the same cached prefix on
// several accounts while its first migration is still streaming.
func (a *admissionControl) claimWeeklyDrain(who admissionSessionKey) (func(), bool) {
	a.mu.Lock()
	if a.sessions[who] == nil && len(a.sessions) >= admissionSessionMax {
		a.pruneSessionsLocked(time.Now())
		if len(a.sessions) >= admissionSessionMax {
			a.mu.Unlock()
			return nil, false
		}
	}
	entry := a.sessionLocked(who, time.Now())
	if entry == nil || entry.weeklyDrainPending {
		a.mu.Unlock()
		return nil, false
	}
	entry.weeklyDrainPending = true
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { a.mu.Lock(); entry.weeklyDrainPending = false; entry.lastSeen = time.Now(); a.mu.Unlock() })
	}, true
}

func (a *upstreamAttempt) finishWeeklyDrain() {
	a.releaseReserved()
	if a.weeklyDrainRelease != nil {
		a.weeklyDrainRelease()
	}
}

func (t usageLimitRetryTransport) preferWeeklyDrainQueue(ctx context.Context, accountID string) bool {
	s := t.server
	if t.skipWeeklyDrain || t.weeklyDrainNewPlacement || t.provider != accounts.ProviderClaude || claudeFableModel(t.poolModel) || s == nil || s.AccountRef == nil || s.CredentialBroker != nil || t.method != http.MethodPost || (t.path != "/messages" && t.path != "/v1/messages") {
		return false
	}
	now := time.Now()
	candidates, _, err := s.claudeDrainCandidates(ctx, t.poolModel, now)
	return err == nil && selectacct.PreferWeeklyDrainQueue(now, accountID, candidates)
}

// beginWeeklyDrain only changes the addressed attempt. Its assignment remains
// provisional until the existing successful-response commit boundary.
func (t usageLimitRetryTransport) beginWeeklyDrain(req *http.Request) (*http.Request, accounts.Account, bool) {
	a, s := t.attempt, t.server
	if a == nil || a.weeklyDrainTried {
		return nil, accounts.Account{}, false
	}
	a.weeklyDrainTried = true
	if req.Method != http.MethodPost || (a.path != "/v1/messages" && a.path != "/messages") {
		return nil, accounts.Account{}, false
	}
	if t.skipWeeklyDrain || claudeFableModel(t.poolModel) || t.provider != accounts.ProviderClaude || s == nil || s.AccountRef == nil || s.admission == nil || s.Sessions == nil || s.CredentialBroker != nil || !a.replayable() || req.Context().Err() != nil {
		return nil, accounts.Account{}, false
	}
	if forced, _, err := session.ExtractAccountIDWithPresence(req); err != nil || forced != "" {
		return nil, accounts.Account{}, false
	}
	expected := a.current().ID
	if t.expectedAccount != "" {
		expected = t.expectedAccount
	} else if assignment, ok := s.Sessions.Get(t.agent, t.session); ok {
		expected = assignment.AccountID
	}
	now := time.Now()
	current := a.current().ID
	if t.weeklyDrainNewPlacement {
		current = ""
	}
	candidates, byID, err := s.claudeDrainCandidates(req.Context(), t.poolModel, now)
	if err != nil {
		return nil, accounts.Account{}, false
	}
	release, claimed := s.admission.claimWeeklyDrain(admissionSessionKey{agent: t.agent, session: t.session})
	if !claimed {
		return nil, accounts.Account{}, false
	}
	keep := false
	defer func() {
		if !keep {
			release()
		}
	}()
	unavailable := make(map[string]bool)
	for len(candidates) > 0 {
		choice, ok := selectacct.ChooseWeeklyDrain(now, current, candidates)
		if !ok {
			return nil, accounts.Account{}, false
		}
		account := byID[choice.AccountID]
		lease, ok := s.admission.tryAcquire(admissionKeyFor(accounts.ProviderClaude, account.ID, t.poolModel), admissionSessionKey{agent: t.agent, session: t.session})
		if !ok {
			unavailable[account.ID] = true
			for i := range candidates {
				if candidates[i].ID == account.ID {
					candidates = append(candidates[:i], candidates[i+1:]...)
					break
				}
			}
			continue
		}
		refreshed, err := s.refreshAccount(req.Context(), account)
		if err != nil || req.Context().Err() != nil || refreshed.ID != account.ID || usageWindowsCredentialKey(refreshed) != usageWindowsCredentialKey(account) {
			lease.release()
			return nil, accounts.Account{}, false
		}
		// Refresh can race policy edits, quota observations or a reset. Read
		// current evidence again before spending a cold conversation prefix.
		now = time.Now()
		latest, latestByID, err := s.claudeDrainCandidates(req.Context(), t.poolModel, now)
		if err != nil {
			lease.release()
			return nil, accounts.Account{}, false
		}
		eligibleCandidates := latest[:0]
		for _, candidate := range latest {
			if !unavailable[candidate.ID] {
				eligibleCandidates = append(eligibleCandidates, candidate)
			}
		}
		rechecked, eligible := selectacct.ChooseWeeklyDrain(now, current, eligibleCandidates)
		if !eligible || rechecked.AccountID != account.ID || usageWindowsCredentialKey(latestByID[account.ID]) != usageWindowsCredentialKey(refreshed) {
			lease.release()
			return nil, accounts.Account{}, false
		}
		nextReq, err := a.replay(req, &refreshed)
		if err != nil {
			lease.release()
			return nil, accounts.Account{}, false
		}
		a.weeklyDrainExpected, a.weeklyDrainTarget, a.weeklyDrainRelease = expected, account.ID, release
		a.reserve(lease)
		keep = true
		return nextReq, refreshed, true
	}
	return nil, accounts.Account{}, false
}

func (s Server) claudeDrainCandidates(ctx context.Context, model string, now time.Time) ([]selectacct.WeeklyDrainAccount, map[string]accounts.Account, error) {
	policies, err := s.accountPolicyCandidatesForProvider(ctx, accounts.ProviderClaude)
	if err != nil {
		return nil, nil, err
	}
	candidates := make([]selectacct.WeeklyDrainAccount, 0, len(policies))
	byID := make(map[string]accounts.Account, len(policies))
	scheduler := s.scheduler().ForModel(model)
	for _, decorated := range policies {
		account := decorated.Account
		if !decorated.Policy.AvailableAt(now) || account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		if s.SchedulerRef != nil {
			if _, blocked := s.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, account.ID, model, now); blocked {
				continue
			}
		}
		if s.admission.paused(admissionKeyFor(accounts.ProviderClaude, account.ID, model), now) {
			continue
		}
		if _, dead := s.AccountRef.terminalCredFailure(account); dead {
			continue
		}
		if scheduler.Exhausted(accounts.ProviderClaude, account.ID) {
			continue
		}
		windows, observed := s.claudeDrainWindows(account, model)
		candidates = append(candidates, selectacct.WeeklyDrainAccount{ID: account.ID, Priority: decorated.Policy.Priority, Windows: windows, ObservedAt: observed, EndsAt: decorated.Policy.EndsAt})
		byID[account.ID] = account
	}
	return candidates, byID, nil
}

// Cached telemetry is evidence only for the credential that observed it. Keep
// the oldest observation when merging model windows; never renew stale quota.
func (s Server) claudeDrainWindows(account accounts.Account, model string) ([]selectacct.LimitWindow, time.Time) {
	ref := s.AccountRef
	ref.usageWindowsMu.Lock()
	defer ref.usageWindowsMu.Unlock()
	entry, ok := ref.usageWindows[account.ID+"\x00"+string(account.Provider)]
	if !ok || entry.credentialKey != usageWindowsCredentialKey(account) {
		return nil, time.Time{}
	}
	observed := entry.at
	if !entry.orderAt.IsZero() && entry.orderAt.Before(observed) {
		observed = entry.orderAt
	}
	windows := append([]accounts.UsageWindow(nil), entry.windows...)
	supplemental := ref.claudeSupplemental[claudeSupplementalCacheKey(account)]
	var applicable []accounts.UsageWindow
	for _, window := range supplemental.windows {
		if window.Feature == "" || selectacct.ModelKey(window.Feature) == selectacct.ModelKey(model) {
			applicable = append(applicable, window)
		}
	}
	if len(applicable) > 0 {
		windows = mergeUsageWindows(windows, applicable)
		at := supplemental.at
		if !supplemental.requestStarted.IsZero() && supplemental.requestStarted.Before(at) {
			at = supplemental.requestStarted
		}
		if at.Before(observed) {
			observed = at
		}
	}
	result := make([]selectacct.LimitWindow, 0, len(windows))
	for _, w := range windows {
		if w.ExtraUsage != nil || (w.Feature != "" && selectacct.ModelKey(w.Feature) != selectacct.ModelKey(model)) {
			continue
		}
		result = append(result, selectacct.LimitWindow{Name: w.Name, UsedPercent: w.UsedPercent, LimitWindowSeconds: w.LimitWindowSeconds, ResetAfterSeconds: w.ResetAfterSeconds, ResetAt: w.ResetAt, Feature: w.Feature})
	}
	return result, observed
}

// A failed discretionary destination does not justify moving the whole
// conversation to whichever account served one fallback request.
func (t usageLimitRetryTransport) keepWarmClaudeDrainSource(accountID string) bool {
	a, s := t.attempt, t.server
	if a == nil || a.weeklyDrainRelease == nil || t.weeklyDrainNewPlacement || accountID == a.weeklyDrainTarget || s == nil {
		return false
	}
	source := a.weeklyDrainExpected
	if s.SchedulerRef != nil {
		if _, blocked := s.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, source, t.poolModel, time.Now()); blocked {
			return false
		}
	}
	if s.scheduler().ForModel(t.poolModel).Exhausted(accounts.ProviderClaude, source) {
		return false
	}
	policies, err := s.accountPolicyCandidatesForProvider(context.Background(), accounts.ProviderClaude)
	if err != nil {
		return false
	}
	for _, candidate := range policies {
		if candidate.Account.ID != source {
			continue
		}
		if !candidate.Policy.AvailableAt(time.Now()) || candidate.Account.AuthMode != accounts.AuthModeOAuth {
			return false
		}
		if _, dead := s.AccountRef.terminalCredFailure(candidate.Account); dead {
			return false
		}
		return true
	}
	return false
}
