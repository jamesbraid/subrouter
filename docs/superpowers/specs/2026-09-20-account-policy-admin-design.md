# Account Policy Administration

## Goal

Allow an administrator to disable, enable, prioritize, and remove accounts in
a running Subrouter pool without editing credential files or logging into the
host. The controls must work through the selected remote server in `sr` and
must not expose credentials.

## Policy model

Account policy is separate from provider credentials and is keyed by provider
plus the canonical account ID returned by `/_subrouter/accounts`. Provider
qualification prevents a Codex and Claude account with the same email or label
from sharing policy accidentally.

- Accounts are enabled by default.
- Accounts have integer priority `0` by default. Higher numbers have higher
  precedence.
- Disabled accounts receive no new work. A sticky assignment to a disabled
  account is ignored and reselected from enabled accounts.
- After normal provider, authentication, quota, model, and avoidance checks,
  routing keeps only candidates in the highest available priority tier. The
  existing quota-aware and session-balancing selector chooses within that tier.
- Forced account selection still rejects a disabled or otherwise unavailable
  account. Preferred selection applies only inside the eligible highest tier.
- Removing an account deletes its credential, policy entry, and session
  assignments that name it, then reloads the live pool.

The policy lives in `account-policy.json` beneath each account store directory.
Writes are atomic, mode `0600`, and serialized with the existing account
mutation transaction. Missing files and missing entries mean enabled with
priority zero. Unknown policy entries are retained so an account can be
temporarily removed and re-enrolled without losing operator intent; explicit
account removal deletes its entry.

## Administrative interface

The admin-authenticated server exposes:

- `GET /_subrouter/accounts` includes `enabled` and `priority` for every row.
- `PATCH /_subrouter/accounts/<provider>/<escaped-id>` accepts exactly one or both of
  `enabled` (boolean) and `priority` (integer from -1000 through 1000).
- `DELETE /_subrouter/accounts/<provider>/<escaped-id>` removes the account from the
  single-tenant server pool using the same durable mutation machinery already
  used for tenant deletion.

Unknown fields, empty patches, invalid priority values, ambiguous IDs, and
missing accounts fail without changing policy or credentials. Policy mutation
reloads the live account snapshot before returning success.

## CLI and dashboard

The selected remote server supports:

```text
sr account disable <provider> <id>
sr account enable <provider> <id>
sr account priority <provider> <id> <integer>
sr account remove <provider> <id>
```

Commands use the server's stored admin credential and print the exact account
ID and resulting state. `sr account list` shows enabled/disabled and priority.
The dashboard account table adds enabled state and priority; it remains
read-only.

## Compatibility and deployment

Existing stores need no migration and preserve current routing because the
default policy is enabled at priority zero. The feature is developed only in
`jamesbraid/subrouter`; no upstream pull request is opened. The homelab role
builds an exact commit from that fork, preserves the current state volume, and
rolls back by restoring the previous repository/version pin.

## Verification

Focused Go tests cover policy persistence and validation, disabled and
priority-aware selection, sticky-session failover, authenticated HTTP mutation,
remote CLI requests, removal cleanup, and dashboard rendering. A local
container deployment is exercised before live cutover. Live verification uses
reversible policy changes on enrolled accounts, confirms disabled accounts are
not selected for new sessions, confirms priority changes selection, and
restores all account policies after the test. Removal is proven against a
temporary disposable account fixture, never one of the nine enrolled accounts.
