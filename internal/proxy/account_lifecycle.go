package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

var errAccountUnavailable = errors.New("account unavailable")

type accountPolicyDatePatch struct {
	present bool
	value   time.Time
}

func (p *accountPolicyDatePatch) UnmarshalJSON(data []byte) error {
	p.present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		p.value = time.Time{}
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return err
	}
	if parsed.IsZero() {
		return errors.New("zero date requires null to clear")
	}
	p.value = parsed.UTC()
	return nil
}

func unavailableAccountPolicy(account accounts.Account, policy accounts.AccountPolicy, now time.Time) error {
	if !policy.EndsAt.IsZero() && !policy.EndsAt.After(now) {
		return fmt.Errorf("%w: account %q ended at %s", errAccountUnavailable, account.ID, policy.EndsAt.UTC().Format(time.RFC3339))
	}
	return fmt.Errorf("%w: account %q is disabled", errAccountUnavailable, account.ID)
}

// A response that has started keeps its lease. Every subsequent physical
// request rechecks local policy, including requests that waited in admission.
func (s Server) accountPolicyForAttempt(ctx context.Context, account accounts.Account, now time.Time) (accounts.AccountPolicy, error) {
	if s.CredentialBroker != nil {
		return accounts.AccountPolicy{Enabled: true}, nil
	}
	for _, candidate := range s.Accounts {
		if candidate.ID == account.ID && accountProviderFor(accountProviderOrCodex(candidate)) == accountProviderFor(accountProviderOrCodex(account)) {
			return accounts.AccountPolicy{Enabled: true}, nil
		}
	}
	if s.AccountRef == nil {
		// Standalone transports and delegated credentials have no local store.
		return accounts.AccountPolicy{Enabled: true}, nil
	}
	s.AccountRef.mu.RLock()
	var managed accounts.Account
	found := false
	for _, candidate := range s.AccountRef.accounts {
		if candidate.ID == account.ID && accountProviderFor(accountProviderOrCodex(candidate)) == accountProviderFor(accountProviderOrCodex(account)) {
			managed, found = candidate, true
			break
		}
	}
	s.AccountRef.mu.RUnlock()
	if !found {
		return accounts.AccountPolicy{}, fmt.Errorf("%w: account %q is no longer available", errAccountUnavailable, account.ID)
	}
	policy := accounts.AccountPolicy{Enabled: true}
	if s.AccountRef.policyStore != nil {
		var err error
		policy, err = s.AccountRef.policyStore.Load(accountProviderOrCodex(managed), managed.ID)
		if err != nil {
			return accounts.AccountPolicy{}, fmt.Errorf("%w: load account policy: %v", errAccountUnavailable, err)
		}
	}
	if !policy.AvailableAt(now) {
		return policy, unavailableAccountPolicy(account, policy, now)
	}
	return policy, nil
}

func (s Server) acquireAccountAdmission(ctx context.Context, key admissionKey, who admissionSessionKey) (*admissionLease, error) {
	account := accounts.Account{ID: key.account, Provider: key.provider}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		policy, err := s.accountPolicyForAttempt(ctx, account, time.Now())
		if err != nil {
			return nil, err
		}
		waitCtx := ctx
		cancel := func() {}
		if !policy.EndsAt.IsZero() {
			waitCtx, cancel = context.WithDeadline(ctx, policy.EndsAt)
		}
		lease, err := s.admission.acquireFor(waitCtx, key, who)
		cancel()
		if parentErr := ctx.Err(); parentErr != nil {
			lease.release()
			return nil, parentErr
		}
		latest, policyErr := s.accountPolicyForAttempt(ctx, account, time.Now())
		if policyErr != nil {
			lease.release()
			return nil, policyErr
		}
		// An admin may renew access while this request waits. Only an obsolete
		// account deadline warrants requeueing; client cancellation still ends it.
		if errors.Is(err, context.DeadlineExceeded) && !policy.EndsAt.IsZero() && (latest.EndsAt.IsZero() || latest.EndsAt.After(policy.EndsAt)) {
			lease.release()
			continue
		}
		return lease, err
	}
}
