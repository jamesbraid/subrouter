package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const qwenSubscriptionHelp = `Qwen.ai Plus/Pro subscriptions:
  sr qwen subscription login <label>
  sr qwen subscription list
  sr qwen subscription remove <label>
  sr qwen subscription quota-login [--fresh] <label>
  sr qwen subscription quota-logout <label>

Accounts are stored on the selected router. Login authorizes an independent
OAuth grant without changing the plain Qwen CLI login. OpenAI-compatible
clients use <router-url>/qwen/v1 with the router's proxy token.
Use /qwen-oauth/v1 to restrict requests to subscription accounts.
Alibaba plan keys and sr qwen login remain separate.
Quota setup opens Qwen's official sign-in in a dedicated Firefox profile.
It stores renewable cookies with broad web-account privileges, not a quota-only key.
Only that new profile's Qwen refresh cookie is read after Firefox exits.
Your existing browser profiles are not accessed. No browser runs after setup.
Use --fresh to discard only a pending enrollment and sign in again.
`

func (r srRunner) qwenSubscription(ctx context.Context, args []string, server *srServerConfig) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(r.out, qwenSubscriptionHelp)
		return nil
	}
	store := qwensubscription.Store{}
	switch args[0] {
	case "quota-login", "quota-logout":
		fresh := args[0] == "quota-login" && len(args) == 3 && args[1] == "--fresh"
		if fresh {
			args = []string{args[0], args[2]}
		}
		if len(args) != 2 {
			return fmt.Errorf("usage: sr qwen subscription %s <label>", args[0])
		}
		label := strings.TrimSpace(args[1])
		if _, err := qwensubscription.ManagedAccountID(label); err != nil {
			return err
		}
		remove := args[0] == "quota-logout"
		if server != nil {
			if err := r.ensureQwenQuotaImport(ctx, *server); err != nil {
				return err
			}
		}
		payload := &serverQwenSubscriptionImport{Label: label, RemoveQuota: remove}
		pendingPath := r.qwenQuotaPendingPath(server, label)
		if remove || fresh {
			if err := removeQwenQuotaPending(pendingPath); err != nil {
				return err
			}
		}
		if !remove {
			fmt.Fprintln(r.out, "This creates a general Qwen web sign-in, not a quota-only grant.")
			fmt.Fprintln(r.out, "The selected router will store its cookies and can use them to access your web account.")
			var email string
			var err error
			if server == nil {
				email, err = store.QuotaAccountEmail(label)
			} else {
				email, err = r.qwenQuotaRemoteEmail(ctx, *server, label)
			}
			if err != nil {
				return err
			}
			if email == "" {
				return fmt.Errorf("verified Qwen subscription %q has no account email", label)
			}
			session, pending, err := readQwenQuotaPending(pendingPath, email)
			if err != nil {
				return err
			}
			if !pending {
				authorize := r.qwenQuotaAuthorize
				if authorize == nil {
					authorize = authorizeQwenQuotaBrowser
				}
				session, err = authorize(ctx, r.out, email)
				if err != nil {
					return err
				}
				if err := saveQwenQuotaPending(pendingPath, email, session); err != nil {
					return err
				}
			}
			payload.QuotaSession = &session
		}
		if server != nil {
			if err := r.postServerAccountImport(ctx, *server, serverAccountImportRequest{Provider: accounts.ProviderQwenOAuth, QwenSubscription: payload}); err != nil {
				if remove {
					return err
				}
				return fmt.Errorf("%w; quota enrollment is retained locally for retry", err)
			}
		} else {
			if err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
				if remove {
					err := store.RemoveQuota(label)
					return err == nil, err
				}
				err := store.EnrollQuota(ctx, r.client, label, *payload.QuotaSession)
				return err == nil, err
			}); err != nil {
				return err
			}
		}
		if remove {
			fmt.Fprintf(r.out, "Removed Qwen quota authorization for %q.\n", label)
		} else {
			if err := removeQwenQuotaPending(pendingPath); err != nil {
				return err
			}
			fmt.Fprintf(r.out, "Added Qwen quota authorization for %q. Run: sr status\n", label)
		}
		return nil
	case "list", "ls":
		if len(args) != 1 {
			return fmt.Errorf("usage: sr qwen subscription list")
		}
		if server != nil {
			all, err := r.fetchServerAccounts(ctx, *server)
			if err != nil {
				return err
			}
			found := false
			for _, acct := range all {
				if acct.Provider == accounts.ProviderQwenOAuth && acct.AuthMode == accounts.AuthModeOAuth {
					found = true
					fmt.Fprintf(r.out, "%s (%s)\n", acct.Label, acct.ID)
				}
			}
			if !found {
				fmt.Fprintln(r.out, "No Qwen.ai subscription accounts configured on the selected server.")
			}
			return nil
		}
		all, err := store.ListAccounts(ctx)
		if err != nil {
			return err
		}
		for _, acct := range all {
			fmt.Fprintf(r.out, "%s (%s)\n", acct.Label, acct.ID)
		}
		if len(all) == 0 {
			fmt.Fprintln(r.out, "No Qwen.ai subscription accounts configured.")
		}
		return nil
	case "login", "add", "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage:\n%s", qwenSubscriptionHelp)
		}
		label := strings.TrimSpace(args[1])
		if _, err := qwensubscription.ManagedAccountID(label); err != nil {
			return err
		}
		remove := args[0] == "remove" || args[0] == "rm"
		if server != nil {
			if err := r.ensureServerAccountImportProviderAvailable(ctx, *server, accounts.ProviderQwenOAuth); err != nil {
				return err
			}
		}
		payload := &serverQwenSubscriptionImport{Label: label, Remove: remove}
		if !remove {
			credential, err := store.AuthorizeManaged(ctx, r.client, label, r.out)
			if err != nil {
				return fmt.Errorf("Qwen subscription login failed: %w", err)
			}
			payload.Credential = credential
		}
		if server != nil {
			if err := r.postServerAccountImport(ctx, *server, serverAccountImportRequest{Provider: accounts.ProviderQwenOAuth, QwenSubscription: payload}); err != nil {
				return err
			}
		} else {
			err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
				if remove {
					_, changed, err := store.RemoveManagedAccount(label)
					if err == nil && !changed {
						return false, fmt.Errorf("Qwen subscription %q was not found", label)
					}
					return changed, err
				}
				_, err := store.SaveManagedCredential(label, payload.Credential)
				return err == nil, err
			})
			if err != nil {
				return err
			}
		}
		if remove {
			fmt.Fprintf(r.out, "Removed Qwen subscription %q.\n", label)
		} else {
			fmt.Fprintf(r.out, "Added isolated Qwen subscription %q. Run: sr status\n", label)
		}
		return nil
	default:
		return fmt.Errorf("unknown Qwen subscription command %q\n%s", args[0], qwenSubscriptionHelp)
	}
}

func (r srRunner) qwenQuotaRemoteEmail(ctx context.Context, server srServerConfig, label string) (string, error) {
	id, _ := qwensubscription.ManagedAccountID(label)
	response, err := r.doServerAccountImportRequestWithQuery(ctx, server, http.MethodGet, nil, url.Values{"account_id": {id}})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var result struct {
		Account struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"qwen_subscription_account"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&result) != nil || result.Account.ID != id || strings.TrimSpace(result.Account.Email) == "" {
		return "", fmt.Errorf("server %s did not return a verified Qwen subscription email; check the account and update the server before sign-in", server.Name)
	}
	return strings.TrimSpace(result.Account.Email), nil
}

func (r srRunner) ensureQwenQuotaImport(ctx context.Context, server srServerConfig) error {
	res, err := r.doServerAccountImportRequest(ctx, server, http.MethodGet, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var capability struct {
		OK      bool `json:"ok"`
		Quota   bool `json:"qwen_subscription_quota"`
		Version int  `json:"qwen_subscription_quota_version"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&capability) != nil || !capability.OK || !capability.Quota || capability.Version != qwensubscription.QuotaSessionVersion {
		return fmt.Errorf("server %s does not support Qwen subscription quota enrollment; update the server before starting web sign-in", server.Name)
	}
	return nil
}
