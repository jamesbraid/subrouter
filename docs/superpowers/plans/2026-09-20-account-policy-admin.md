# Account Policy Administration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add persistent enable/disable, integer priority, and remote removal controls for Subrouter accounts.

**Architecture:** A credential-independent policy store decorates live account snapshots. Routing filters disabled accounts and then restricts eligible candidates to the highest priority tier before invoking existing selection. Admin HTTP endpoints and `sr account` commands mutate policy or remove credentials through existing account transactions.

**Tech Stack:** Go standard library, existing Subrouter account stores and HTTP client, table-driven Go tests.

**Spec:** `docs/superpowers/specs/2026-09-20-account-policy-admin-design.md`

## Global Constraints

- Missing policy means enabled with priority zero.
- Priority is an integer from -1000 through 1000.
- Credential payloads and complete authorization headers are never logged.
- Existing sticky sessions remain sticky unless their account is disabled or removed.
- No upstream pull request.

## Review Focus

- Concurrent policy writes must not lose updates or publish partial JSON.
- A disabled forced account must fail rather than silently route elsewhere.
- A stale sticky assignment must not bypass disabled state.
- Removing one provider's account must not remove another provider sharing a label.
- Invalid or ambiguous IDs must leave credentials and policy untouched.

---

### Task 1: Persistent account policy store

**Files:** create `internal/accounts/policy.go`; create `internal/accounts/policy_test.go`; modify `account/account.go` only if policy fields belong on the public account value.

**Interfaces:** produce `AccountPolicy{Enabled bool, Priority int}`, load/list/update/delete methods keyed by provider plus canonical ID, atomic mode-0600 persistence, and helpers that decorate/filter account slices. Higher numeric priority wins.

- [ ] Write table-driven failing tests for defaults, round-trip, invalid priority, atomic replacement, delete, and concurrent updates.
- [ ] Run the focused tests and confirm the intended failures.
- [ ] Implement the smallest policy store satisfying those tests.
- [ ] Run the focused tests to green.
- [ ] Commit the storage slice.

### Task 2: Routing policy

**Files:** modify `internal/proxy/proxy.go`, `internal/proxy/tenant_credential_lease.go`, and focused routing tests beside the existing selectors.

**Interfaces:** consume decorated account policy; produce eligible candidates containing only enabled accounts in the highest available priority tier.

- [ ] Write failing tests for disabled exclusion, stale sticky reassignment, forced-disabled failure, preferred-tier behavior, priority ordering, and unchanged default routing.
- [ ] Run the focused tests and confirm the intended failures.
- [ ] Apply policy before sticky, preferred, and scheduler selection without replacing existing quota/auth/model gates.
- [ ] Run the focused tests to green.
- [ ] Commit the routing slice.

### Task 3: Admin HTTP mutation and removal

**Files:** modify `internal/proxy/proxy.go`, reuse/refactor deletion helpers from `internal/proxy/multitenant.go`, and add focused HTTP tests.

**Interfaces:** produce authenticated `PATCH` and `DELETE /_subrouter/accounts/<provider>/<id>` and enriched account list rows.

- [ ] Write failing tests for authentication, patch validation, missing and ambiguous IDs, successful live reload, durable deletion, policy cleanup, and session cleanup.
- [ ] Run the focused tests and confirm the intended failures.
- [ ] Implement mutations under the existing account transaction and publish/reload sequence.
- [ ] Run the focused tests to green.
- [ ] Commit the HTTP slice.

### Task 4: Remote CLI and dashboard

**Files:** modify `internal/broker/client.go`, `cmd/subrouter/sr_cloud.go`, help text, dashboard templates, and focused client/CLI/dashboard tests.

**Interfaces:** consume the HTTP API; produce `sr account enable|disable|priority|remove` and visible account policy columns.

- [ ] Write failing transport and output tests for every command and list/dashboard rendering.
- [ ] Run the focused tests and confirm the intended failures.
- [ ] Implement client methods, command dispatch, help, and rendering.
- [ ] Run the focused tests to green.
- [ ] Commit the operator slice.

### Task 5: Integration and fork release

**Files:** update README operator documentation and any changelog/version files required by repository convention.

**Interfaces:** produce one reviewed fork commit/tag suitable for the homelab's exact deployment pin.

- [ ] Run `gofmt` on changed Go files.
- [ ] Run focused packages, then `go test ./...` with untruncated exit-status-preserving output.
- [ ] Run a second-agent whole-branch review and fix findings.
- [ ] Push the feature branch to `jamesbraid/subrouter`; do not open an upstream PR.
- [ ] Record the exact tested commit.

### Task 6: Homelab deployment and live acceptance

**Files:** update the Ansible Subrouter role repository and exact revision pin; update its runbook.

**Interfaces:** deploy the tested fork build while preserving `/home/infra/subrouter/state` and all enrolled accounts.

- [ ] Build and exercise the fork in an isolated local container with fixture accounts.
- [ ] Update and syntax-check the Ansible role.
- [ ] Deploy only the Subrouter role to Voodoo and verify idempotence.
- [ ] Verify health, all nine enrolled accounts, CLI list policy, reversible disable/enable, reversible priority selection, disposable-account removal, dashboard columns, Claude, and Codex proxy traffic.
- [ ] Push the Ansible change and monitor CI to terminal success.
