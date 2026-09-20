package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/manaflow-ai/subrouter/account"
)

const (
	MinPolicyPriority = -1000
	MaxPolicyPriority = 1000
)

// AccountPolicy contains operator-controlled routing state. It is deliberately
// separate from account.Account so policy storage never handles credentials.
type AccountPolicy struct {
	Enabled  bool `json:"enabled"`
	Priority int  `json:"priority"`
}

// PolicyKey identifies a policy by the provider-qualified canonical account ID.
// Provider qualification prevents accounts from different providers that share
// an email or label from sharing policy accidentally.
type PolicyKey struct {
	Provider  account.Provider
	AccountID string
}

// AccountWithPolicy is a credential-bearing account decorated with its
// credential-independent policy for one immutable routing snapshot.
type AccountWithPolicy struct {
	Account account.Account
	Policy  AccountPolicy
}

// PriorityValidationError reports a priority outside the supported range.
type PriorityValidationError struct {
	Priority int
}

func (e *PriorityValidationError) Error() string {
	return fmt.Sprintf("account policy priority %d is outside %d..%d", e.Priority, MinPolicyPriority, MaxPolicyPriority)
}

// PolicyStore persists account policies in one JSON document.
type PolicyStore struct {
	path                 string
	syncDirectoryForTest func(string) error
}

type policyDocument struct {
	Policies []policyEntry `json:"policies"`
}

type policyEntry struct {
	Provider  account.Provider `json:"provider"`
	AccountID string           `json:"account_id"`
	Policy    AccountPolicy    `json:"policy"`
}

const policyLockIdentifier = "account-policy"

// NewPolicyStore opens path. A missing file is an empty store, where every
// account has the default enabled, zero-priority policy.
func NewPolicyStore(path string) (*PolicyStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("account policy path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve account policy path: %w", err)
	}
	canonicalPath, err := resolveStoreAuthorityPath(absPath)
	if err != nil {
		return nil, fmt.Errorf("resolve account policy path: %w", err)
	}
	store := &PolicyStore{path: filepath.Clean(canonicalPath)}
	_, err = store.read()
	if err != nil {
		return nil, err
	}
	return store, nil
}

// Load returns the policy for provider and the canonical account ID. Missing
// entries use the compatibility default: enabled with priority zero.
func (s *PolicyStore) Load(provider account.Provider, accountID string) (AccountPolicy, error) {
	key, err := newPolicyKey(provider, accountID)
	if err != nil {
		return AccountPolicy{}, err
	}
	policies, err := s.read()
	if err != nil {
		return AccountPolicy{}, err
	}
	if policy, ok := policies[key]; ok {
		return policy, nil
	}
	return defaultAccountPolicy(), nil
}

// List returns the explicitly stored policies. Missing entries are omitted
// because callers can obtain their default by calling Load.
func (s *PolicyStore) List() (map[PolicyKey]AccountPolicy, error) {
	policies, err := s.read()
	if err != nil {
		return nil, err
	}
	return clonePolicies(policies), nil
}

// Update replaces the policy for provider and the canonical account ID.
func (s *PolicyStore) Update(provider account.Provider, accountID string, policy AccountPolicy) error {
	key, err := newPolicyKey(provider, accountID)
	if err != nil {
		return err
	}
	if err := validatePolicy(policy); err != nil {
		return err
	}
	return s.withPolicyLock(func() error {
		policies, err := s.readUnlocked()
		if err != nil {
			return err
		}
		policies[key] = policy
		return s.writeUnlocked(policies)
	})
}

// Delete removes the explicit policy for provider and the canonical account
// ID. A later Load therefore returns the default policy.
func (s *PolicyStore) Delete(provider account.Provider, accountID string) error {
	key, err := newPolicyKey(provider, accountID)
	if err != nil {
		return err
	}
	return s.withPolicyLock(func() error {
		policies, err := s.readUnlocked()
		if err != nil {
			return err
		}
		if _, ok := policies[key]; !ok {
			return nil
		}
		delete(policies, key)
		return s.writeUnlocked(policies)
	})
}

// Decorate applies stored policy to a live account snapshot. Account IDs are
// taken from Account.ID, the canonical ID used by the account listing API.
func (s *PolicyStore) Decorate(accounts []account.Account) ([]AccountWithPolicy, error) {
	policies, err := s.List()
	if err != nil {
		return nil, err
	}
	decorated := make([]AccountWithPolicy, 0, len(accounts))
	for _, acct := range accounts {
		policy := defaultAccountPolicy()
		if stored, ok := policies[PolicyKey{Provider: acct.Provider, AccountID: acct.ID}]; ok {
			policy = stored
		}
		decorated = append(decorated, AccountWithPolicy{Account: acct, Policy: policy})
	}
	return decorated, nil
}

// FilterEnabled removes disabled accounts while preserving input order.
func FilterEnabled(accounts []AccountWithPolicy) []AccountWithPolicy {
	filtered := make([]AccountWithPolicy, 0, len(accounts))
	for _, acct := range accounts {
		if acct.Policy.Enabled {
			filtered = append(filtered, acct)
		}
	}
	return filtered
}

// FilterEligible keeps enabled accounts in the highest available priority
// tier. Existing quota-aware selection remains responsible for choosing within
// that tier.
func FilterEligible(accounts []AccountWithPolicy) []AccountWithPolicy {
	filtered := FilterEnabled(accounts)
	if len(filtered) == 0 {
		return filtered
	}
	highest := filtered[0].Policy.Priority
	for _, acct := range filtered[1:] {
		if acct.Policy.Priority > highest {
			highest = acct.Policy.Priority
		}
	}
	eligible := filtered[:0]
	for _, acct := range filtered {
		if acct.Policy.Priority == highest {
			eligible = append(eligible, acct)
		}
	}
	return eligible
}

func defaultAccountPolicy() AccountPolicy {
	return AccountPolicy{Enabled: true}
}

func newPolicyKey(provider account.Provider, accountID string) (PolicyKey, error) {
	if strings.TrimSpace(string(provider)) == "" {
		return PolicyKey{}, errors.New("account policy provider is required")
	}
	if strings.TrimSpace(accountID) == "" {
		return PolicyKey{}, errors.New("account policy account ID is required")
	}
	return PolicyKey{Provider: provider, AccountID: accountID}, nil
}

func validatePolicy(policy AccountPolicy) error {
	if policy.Priority < MinPolicyPriority || policy.Priority > MaxPolicyPriority {
		return &PriorityValidationError{Priority: policy.Priority}
	}
	return nil
}

func (s *PolicyStore) read() (map[PolicyKey]AccountPolicy, error) {
	lock, err := s.acquireLock()
	if err != nil {
		return nil, err
	}
	policies, readErr := s.readUnlocked()
	return policies, errors.Join(readErr, lock.Close())
}

func (s *PolicyStore) readUnlocked() (map[PolicyKey]AccountPolicy, error) {
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[PolicyKey]AccountPolicy), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read account policy: %w", err)
	}
	var document policyDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("parse account policy: %w", err)
	}
	policies := make(map[PolicyKey]AccountPolicy, len(document.Policies))
	for _, entry := range document.Policies {
		key, err := newPolicyKey(entry.Provider, entry.AccountID)
		if err != nil {
			return nil, fmt.Errorf("parse account policy entry: %w", err)
		}
		if err := validatePolicy(entry.Policy); err != nil {
			return nil, fmt.Errorf("parse account policy entry %s/%s: %w", entry.Provider, entry.AccountID, err)
		}
		if _, exists := policies[key]; exists {
			return nil, fmt.Errorf("parse account policy: duplicate entry for %s/%s", entry.Provider, entry.AccountID)
		}
		policies[key] = entry.Policy
	}
	return policies, nil
}

func (s *PolicyStore) writeUnlocked(policies map[PolicyKey]AccountPolicy) error {
	entries := make([]policyEntry, 0, len(policies))
	for key, policy := range policies {
		entries = append(entries, policyEntry{Provider: key.Provider, AccountID: key.AccountID, Policy: policy})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Provider != entries[j].Provider {
			return entries[i].Provider < entries[j].Provider
		}
		return entries[i].AccountID < entries[j].AccountID
	})
	document := policyDocument{Policies: entries}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode account policy: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create account policy directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("stage account policy: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("restrict staged account policy: %w", err)
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write staged account policy: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync staged account policy: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close staged account policy: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("publish account policy: %w", err)
	}
	cleanup = false
	syncDirectory := s.syncDirectoryForTest
	if syncDirectory == nil {
		syncDirectory = syncPolicyDirectory
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync account policy directory: %w", err)
	}
	return nil
}

func (s *PolicyStore) acquireLock() (*accountFileLock, error) {
	return (CodexStore{Dir: filepath.Dir(s.path)}).lockStoredAccount(policyLockIdentifier)
}

func (s *PolicyStore) withPolicyLock(fn func() error) error {
	lock, err := s.acquireLock()
	if err != nil {
		return err
	}
	return errors.Join(fn(), lock.Close())
}

func syncPolicyDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func clonePolicies(policies map[PolicyKey]AccountPolicy) map[PolicyKey]AccountPolicy {
	clone := make(map[PolicyKey]AccountPolicy, len(policies))
	for key, policy := range policies {
		clone[key] = policy
	}
	return clone
}
