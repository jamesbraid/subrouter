package accounts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

func TestPolicyStoreDefaultsAndProviderQualifiedIdentity(t *testing.T) {
	store := newTestPolicyStore(t)

	got, err := store.Load(account.ProviderCodex, "shared@example.com")
	if err != nil {
		t.Fatalf("load missing policy: %v", err)
	}
	if got != (AccountPolicy{Enabled: true}) {
		t.Fatalf("missing policy = %+v, want enabled priority zero", got)
	}

	if err := store.Update(account.ProviderClaude, "shared@example.com", AccountPolicy{Enabled: false, Priority: 7}); err != nil {
		t.Fatalf("update Claude policy: %v", err)
	}
	codex, err := store.Load(account.ProviderCodex, "shared@example.com")
	if err != nil {
		t.Fatalf("reload Codex policy: %v", err)
	}
	if codex != (AccountPolicy{Enabled: true}) {
		t.Fatalf("Codex policy = %+v, provider-qualified ID was not isolated", codex)
	}
}

func TestPolicyStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account-policy.json")
	store, err := NewPolicyStore(path)
	if err != nil {
		t.Fatalf("new policy store: %v", err)
	}
	want := AccountPolicy{Enabled: false, Priority: -42}
	if err := store.Update(account.ProviderCodex, "codex-account", want); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	reloaded, err := NewPolicyStore(path)
	if err != nil {
		t.Fatalf("reload policy store: %v", err)
	}
	got, err := reloaded.Load(account.ProviderCodex, "codex-account")
	if err != nil {
		t.Fatalf("load persisted policy: %v", err)
	}
	if got != want {
		t.Fatalf("persisted policy = %+v, want %+v", got, want)
	}
}

func TestPolicyStoreRejectsInvalidPriorityWithoutChangingState(t *testing.T) {
	store := newTestPolicyStore(t)
	if err := store.Update(account.ProviderCodex, "codex-account", AccountPolicy{Enabled: false, Priority: 3}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	for _, test := range []struct {
		name     string
		priority int
	}{
		{name: "below minimum", priority: -1001},
		{name: "above maximum", priority: 1001},
	} {
		t.Run(test.name, func(t *testing.T) {
			priority := test.priority
			err := store.Update(account.ProviderCodex, "codex-account", AccountPolicy{Enabled: true, Priority: priority})
			if err == nil {
				t.Fatalf("priority %d was accepted", priority)
			}
			var validationErr *PriorityValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("priority %d error = %v, want PriorityValidationError", priority, err)
			}
			got, loadErr := store.Load(account.ProviderCodex, "codex-account")
			if loadErr != nil {
				t.Fatalf("load after invalid update: %v", loadErr)
			}
			if got != (AccountPolicy{Enabled: false, Priority: 3}) {
				t.Fatalf("state after priority %d = %+v, want original policy", priority, got)
			}
		})
	}
}

func TestPolicyStoreAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account-policy.json")
	store, err := NewPolicyStore(path)
	if err != nil {
		t.Fatalf("new policy store: %v", err)
	}
	if err := store.Update(account.ProviderCodex, "codex-account", AccountPolicy{Enabled: true, Priority: 1}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}

	var wg sync.WaitGroup
	readErrs := make(chan error, 100)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			body, err := os.ReadFile(path)
			if err != nil {
				readErrs <- err
				return
			}
			var document policyDocument
			if err := json.Unmarshal(body, &document); err != nil {
				readErrs <- err
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		if err := store.Update(account.ProviderCodex, "codex-account", AccountPolicy{Enabled: i%2 == 0, Priority: i}); err != nil {
			t.Fatalf("atomic update %d: %v", i, err)
		}
	}
	wg.Wait()
	close(readErrs)
	for err := range readErrs {
		t.Fatalf("reader observed partial policy file: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat policy file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("policy file mode = %04o, want 0600", got)
	}
}

func TestPolicyStoreDeleteRestoresDefaultAndListOmitsEntry(t *testing.T) {
	store := newTestPolicyStore(t)
	if err := store.Update(account.ProviderCodex, "codex-account", AccountPolicy{Enabled: false, Priority: 4}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	if err := store.Delete(account.ProviderCodex, "codex-account"); err != nil {
		t.Fatalf("delete policy: %v", err)
	}

	got, err := store.Load(account.ProviderCodex, "codex-account")
	if err != nil {
		t.Fatalf("load deleted policy: %v", err)
	}
	if got != (AccountPolicy{Enabled: true}) {
		t.Fatalf("deleted policy = %+v, want default", got)
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("listed policies after delete = %+v, want empty", list)
	}
}

func TestPolicyStoreConcurrentUpdatesPreserveAllEntries(t *testing.T) {
	store := newTestPolicyStore(t)
	const count = 32
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- store.Update(account.ProviderCodex, "account-"+string(rune('a'+i)), AccountPolicy{Enabled: i%2 == 0, Priority: i})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("list after concurrent updates: %v", err)
	}
	if len(list) != count {
		t.Fatalf("concurrent policy count = %d, want %d: %+v", len(list), count, list)
	}
}

func TestPolicyStoreIndependentAliasesPreserveConcurrentUpdates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink aliases require elevated privileges on Windows")
	}
	realDir := t.TempDir()
	aliasRoot := t.TempDir()
	aliasDir := filepath.Join(aliasRoot, "store")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Fatalf("symlink policy store directory: %v", err)
	}

	first, err := NewPolicyStore(filepath.Join(realDir, "account-policy.json"))
	if err != nil {
		t.Fatalf("new real-path policy store: %v", err)
	}
	second, err := NewPolicyStore(filepath.Join(aliasDir, "account-policy.json"))
	if err != nil {
		t.Fatalf("new alias-path policy store: %v", err)
	}

	const count = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := first.Update(account.ProviderCodex, "real-"+string(rune('a'+i)), AccountPolicy{Enabled: true, Priority: i}); err != nil {
				t.Errorf("real-path update %d: %v", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := second.Update(account.ProviderClaude, "alias-"+string(rune('a'+i)), AccountPolicy{Enabled: false, Priority: -i}); err != nil {
				t.Errorf("alias-path update %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	list, err := first.List()
	if err != nil {
		t.Fatalf("list policies after aliased updates: %v", err)
	}
	if len(list) != count*2 {
		t.Fatalf("aliased concurrent policy count = %d, want %d", len(list), count*2)
	}
}

func TestPolicyStoreReportsDirectorySyncFailureAfterRename(t *testing.T) {
	store := newTestPolicyStore(t)
	if err := store.Update(account.ProviderCodex, "codex-account", AccountPolicy{Enabled: true, Priority: 1}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	wantErr := errors.New("directory sync failed")
	store.syncDirectoryForTest = func(string) error { return wantErr }

	err := store.Update(account.ProviderCodex, "codex-account", AccountPolicy{Enabled: false, Priority: 2})
	if !errors.Is(err, wantErr) {
		t.Fatalf("update error = %v, want directory sync error", err)
	}

	reloaded, reloadErr := NewPolicyStore(store.path)
	if reloadErr != nil {
		t.Fatalf("reload after post-rename sync failure: %v", reloadErr)
	}
	got, loadErr := reloaded.Load(account.ProviderCodex, "codex-account")
	if loadErr != nil {
		t.Fatalf("load after post-rename sync failure: %v", loadErr)
	}
	if got != (AccountPolicy{Enabled: false, Priority: 2}) {
		t.Fatalf("policy after post-rename sync failure = %+v, want renamed policy", got)
	}
}

func TestDecorateAndFilterAccounts(t *testing.T) {
	store := newTestPolicyStore(t)
	if err := store.Update(account.ProviderClaude, "shared", AccountPolicy{Enabled: false, Priority: 9}); err != nil {
		t.Fatalf("disable Claude account: %v", err)
	}
	if err := store.Update(account.ProviderCodex, "preferred", AccountPolicy{Enabled: true, Priority: 9}); err != nil {
		t.Fatalf("prioritize Codex account: %v", err)
	}
	if err := store.Update(account.ProviderCodex, "lower", AccountPolicy{Enabled: true, Priority: 1}); err != nil {
		t.Fatalf("seed lower-priority account: %v", err)
	}

	decorated, err := store.Decorate([]account.Account{
		{Provider: account.ProviderClaude, ID: "shared"},
		{Provider: account.ProviderCodex, ID: "preferred"},
		{Provider: account.ProviderCodex, ID: "lower"},
		{Provider: account.ProviderCodex, ID: "default"},
	})
	if err != nil {
		t.Fatalf("decorate accounts: %v", err)
	}
	eligible := FilterEligible(decorated)
	if len(eligible) != 1 || eligible[0].Account.ID != "preferred" {
		t.Fatalf("eligible accounts = %+v, want only highest enabled priority", eligible)
	}
	if decorated[3].Policy != (AccountPolicy{Enabled: true}) {
		t.Fatalf("missing policy decoration = %+v, want default", decorated[3].Policy)
	}
}

func newTestPolicyStore(t *testing.T) *PolicyStore {
	t.Helper()
	store, err := NewPolicyStore(filepath.Join(t.TempDir(), "account-policy.json"))
	if err != nil {
		t.Fatalf("new policy store: %v", err)
	}
	return store
}
