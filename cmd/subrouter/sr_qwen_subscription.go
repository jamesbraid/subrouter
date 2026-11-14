package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const qwenSubscriptionHelp = `Qwen.ai Plus/Pro subscriptions:
  sr qwen subscription login <label>
  sr qwen subscription list
  sr qwen subscription remove <label>
  sr qwen subscription quota-login <label>
  sr qwen subscription quota-logout <label>

Accounts are stored on the selected router. Login authorizes an independent
OAuth grant without changing the plain Qwen CLI login. OpenAI-compatible
clients use <router-url>/qwen-oauth/v1 with the router's proxy token.
Alibaba plan keys and sr qwen login remain separate.
Quota setup accepts a separate web Authorization value with hidden input.
After setup the router polls quota headlessly; no browser stays running.
`

func (r srRunner) qwenSubscription(ctx context.Context, args []string, server *srServerConfig) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(r.out, qwenSubscriptionHelp)
		return nil
	}
	store := qwensubscription.Store{}
	switch args[0] {
	case "quota-login", "quota-logout":
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
		if !remove {
			fmt.Fprintln(r.out, "Sign into https://chat.qwen.ai/settings/usage-billing in your browser.")
			fmt.Fprintln(r.out, "In Developer Tools → Network, reload and select the agent/usage request.")
			fmt.Fprintln(r.out, "Copy its Authorization request header. This web credential is stored only for quota reads; it is broader than a quota-only key.")
			token, err := promptSecret(r.out, bufio.NewReader(r.in), r.in, "Qwen web Authorization (hidden): ")
			if err != nil {
				return err
			}
			if strings.TrimSpace(token) == "" {
				return fmt.Errorf("Qwen quota authorization is required")
			}
			payload.QuotaToken = &token
		}
		if server != nil {
			if err := r.postServerAccountImport(ctx, *server, serverAccountImportRequest{Provider: accounts.ProviderQwenOAuth, QwenSubscription: payload}); err != nil {
				return err
			}
		} else {
			if err := proxy.PublishAccountDiskMutation(ctx, r.store.StoreDir(), func() (bool, error) {
				if remove {
					err := store.RemoveQuota(label)
					return err == nil, err
				}
				err := store.EnrollQuota(ctx, r.client, label, *payload.QuotaToken)
				return err == nil, err
			}); err != nil {
				return err
			}
		}
		if remove {
			fmt.Fprintf(r.out, "Removed Qwen quota authorization for %q.\n", label)
		} else {
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

func (r srRunner) ensureQwenQuotaImport(ctx context.Context, server srServerConfig) error {
	res, err := r.doServerAccountImportRequest(ctx, server, http.MethodGet, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var capability struct {
		OK    bool `json:"ok"`
		Quota bool `json:"qwen_subscription_quota"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&capability) != nil || !capability.OK || !capability.Quota {
		return fmt.Errorf("server %s does not support Qwen subscription quota enrollment; update the server before entering a web credential", server.Name)
	}
	return nil
}
