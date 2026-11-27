package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func usageGridClaudeLimitResetCell(row srUsageRow) usageGridCell {
	if usageProvider(row) != accounts.ProviderClaude || row.authMode != accounts.AuthModeOAuth {
		return usageGridCell{}
	}
	status := row.limitResets
	if status == nil {
		return usageGridCell{}
	}
	if !status.Known {
		if status != nil && status.Status == "scope_required" {
			return usageGridCell{Text: "needs profile", Style: ansiDim}
		}
		return usageGridCell{Text: "unknown", Style: ansiDim}
	}
	if status.Eligible != nil && !*status.Eligible {
		return usageGridCell{Text: "not eligible", Style: ansiDim}
	}
	var parts []string
	now := time.Now()
	for _, grant := range status.Grants {
		if grant.Remaining == 0 {
			continue
		}
		if end, err := time.Parse(time.RFC3339, grant.ExpiresAt); err == nil && !now.Before(end) {
			continue
		}
		label := grant.Label
		if label == "" {
			label = grant.Kind
		}
		if label == "" {
			label = strings.Join(grant.Clears, "+")
		}
		if label == "" {
			label = "reset"
		}
		// Keep the count and availability before a provider label that may be
		// a full promotional sentence and truncated by the status table.
		text := fmt.Sprintf("%d", grant.Remaining)
		if start, err := time.Parse(time.RFC3339, grant.StartsAt); err == nil && now.Before(start) {
			text += " pending"
		} else if grant.Paused {
			text += " paused"
		} else if !grant.UsableNow {
			text += " held"
		}
		if end, err := time.Parse(time.RFC3339, grant.ExpiresAt); err == nil {
			text += "/" + strings.ReplaceAll(formatDuration(int64(end.Sub(now).Seconds())), " ", "")
		}
		parts = append(parts, text+" "+label)
	}
	if len(parts) == 0 {
		return usageGridCell{Text: "none", Style: ansiDim}
	}
	return usageGridCell{Text: strings.Join(parts, ", "), Style: ansiGreen}
}
