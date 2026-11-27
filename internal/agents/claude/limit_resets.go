package claude

import (
	"encoding/json"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// LimitResetsFromUsage reads the same optional status block as Claude Code's
// /limit-reset command. The usage request opts in with cedar_ember=1 with
// the normal spend fields retained. Claiming uses a separate POST which we
// never issue.
func LimitResetsFromUsage(usage *UsageResponse) *accounts.LimitResetStatus {
	unknown := &accounts.LimitResetStatus{Status: "unavailable"}
	if usage == nil {
		return unknown
	}
	var raw struct {
		Eligible       *bool             `json:"eligible"`
		Reason         string            `json:"ineligible_reason"`
		AtLimit        bool              `json:"at_limit"`
		WeeklyResetsAt string            `json:"weekly_resets_at"`
		CooldownUntil  string            `json:"cooldown_until"`
		Grants         []json.RawMessage `json:"grants"`
	}
	if json.Unmarshal(usage.LimitResets, &raw) != nil || raw.Eligible == nil || *raw.Eligible && raw.Grants == nil {
		return unknown
	}
	out := &accounts.LimitResetStatus{Known: true, Status: "live", Eligible: raw.Eligible, Reason: raw.Reason, AtLimit: raw.AtLimit, WeeklyResetsAt: raw.WeeklyResetsAt, CooldownUntil: raw.CooldownUntil}
	for _, body := range raw.Grants {
		var grant struct {
			ID              string   `json:"id"`
			Label           string   `json:"label"`
			Remaining       *int     `json:"resets_left"`
			Total           int      `json:"resets_total"`
			StartsAt        string   `json:"starts_at"`
			ExpiresAt       string   `json:"ends_at"`
			Clears          []string `json:"clears"`
			Paused          bool     `json:"paused"`
			UsableNow       bool     `json:"usable_now"`
			RequiresLimit   *bool    `json:"use_requires_limit"`
			Kind            string   `json:"type"`
			GivesBackWeekly bool     `json:"gives_back_weekly"`
			Reason          string   `json:"unavailable_reason"`
		}
		if json.Unmarshal(body, &grant) != nil || grant.ID == "" || grant.Remaining == nil || *grant.Remaining < 0 || grant.Total < 0 {
			return unknown
		}
		requiresLimit := true
		if grant.RequiresLimit != nil {
			requiresLimit = *grant.RequiresLimit
		}
		out.Grants = append(out.Grants, accounts.LimitResetGrant{ID: grant.ID, Label: grant.Label, Remaining: *grant.Remaining, Total: grant.Total, StartsAt: grant.StartsAt, ExpiresAt: grant.ExpiresAt, Clears: grant.Clears, Paused: grant.Paused, UsableNow: grant.UsableNow, RequiresLimit: requiresLimit, Kind: grant.Kind, GivesBackWeekly: grant.GivesBackWeekly, UnavailableReason: grant.Reason})
	}
	return out
}
