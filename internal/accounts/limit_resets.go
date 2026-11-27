package accounts

// LimitResetStatus describes provider-granted manual resets. It is display-only;
// reading this metadata never redeems a grant or changes quota routing.
type LimitResetStatus struct {
	Known          bool              `json:"known"`
	Status         string            `json:"status"`
	Eligible       *bool             `json:"eligible,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	AtLimit        bool              `json:"at_limit,omitempty"`
	Grants         []LimitResetGrant `json:"grants,omitempty"`
	WeeklyResetsAt string            `json:"weekly_resets_at,omitempty"`
	CooldownUntil  string            `json:"cooldown_until,omitempty"`
}

// LimitResetGrant retains the provider's scope: a Fable grant may return only
// Fable's spend to weekly quota, while a full grant clears different windows.
type LimitResetGrant struct {
	ID                string   `json:"id"`
	Label             string   `json:"label,omitempty"`
	Remaining         int      `json:"remaining"`
	Total             int      `json:"total"`
	StartsAt          string   `json:"starts_at,omitempty"`
	ExpiresAt         string   `json:"expires_at,omitempty"`
	Clears            []string `json:"clears,omitempty"`
	Paused            bool     `json:"paused,omitempty"`
	UsableNow         bool     `json:"usable_now"`
	RequiresLimit     bool     `json:"requires_limit"`
	Kind              string   `json:"kind,omitempty"`
	GivesBackWeekly   bool     `json:"gives_back_weekly,omitempty"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}
