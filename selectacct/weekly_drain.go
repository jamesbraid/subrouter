package selectacct

import (
	"math"
	"time"
)

// WeeklyDrainAccount contains measured quota for one routable Claude
// subscription. Callers exclude disabled accounts, failed credentials and
// admission cooldowns, and include only windows applying to the request model.
type WeeklyDrainAccount struct {
	ID         string
	Priority   int
	Windows    []LimitWindow
	ObservedAt time.Time
	EndsAt     time.Time
}

type WeeklyDrainChoice struct {
	AccountID string
	// ResetAt is the next quota-loss deadline: the weekly reset or an
	// earlier known account end.
	ResetAt time.Time
}

const weeklyDrainFreshness = 2 * time.Minute

// ChooseWeeklyDrain chooses expiring subscription quota without considering
// placement load. The caller must reserve real admission capacity before using
// the choice and persist a session move only after the upstream succeeds.
// Healthy stickies move only toward an earlier quota-loss deadline, so
// fluctuating utilization cannot bounce a conversation between accounts.
func ChooseWeeklyDrain(now time.Time, currentID string, accounts []WeeklyDrainAccount) (WeeklyDrainChoice, bool) {
	if len(accounts) == 0 {
		return WeeklyDrainChoice{}, false
	}
	priority, hasPriority := 0, false
	for _, account := range accounts {
		if !account.EndsAt.IsZero() && !account.EndsAt.After(now) {
			continue
		}
		if !hasPriority || account.Priority > priority {
			priority = account.Priority
			hasPriority = true
		}
	}
	if !hasPriority {
		return WeeklyDrainChoice{}, false
	}
	var current weeklyDrainEvidence
	if currentID != "" {
		found := false
		for _, account := range accounts {
			if account.ID == currentID {
				current = inspectWeeklyDrain(now, account)
				found = true
				break
			}
		}
		// Missing telemetry is not evidence that moving a healthy sticky is
		// worth losing its upstream prompt cache. Hard failover is separate.
		if !found || !current.valid {
			return WeeklyDrainChoice{}, false
		}
	}
	var choice WeeklyDrainChoice
	bestSurplus := 0.0
	for _, account := range accounts {
		if account.ID == "" || account.ID == currentID || account.Priority != priority {
			continue
		}
		evidence := inspectWeeklyDrain(now, account)
		if !evidence.healthy || evidence.urgentReset.IsZero() {
			continue
		}
		if current.healthy && !evidence.urgentReset.Before(current.firstWeeklyReset) {
			continue
		}
		if choice.AccountID == "" || evidence.urgentReset.Before(choice.ResetAt) ||
			(evidence.urgentReset.Equal(choice.ResetAt) && (evidence.surplus > bestSurplus ||
				(evidence.surplus == bestSurplus && account.ID < choice.AccountID))) {
			choice = WeeklyDrainChoice{AccountID: account.ID, ResetAt: evidence.urgentReset}
			bestSurplus = evidence.surplus
		}
	}
	return choice, choice.AccountID != ""
}

// PreferWeeklyDrainQueue keeps a warm request on expiring quota rather than
// proactively spilling it to another account. It does not bypass admission:
// callers still enforce bounded queues, cancellation and actual rejections.
// A conversation can spend the last quota while every applicable window
// remains positive and weekly quota is ahead of pace.
func PreferWeeklyDrainQueue(now time.Time, currentID string, accounts []WeeklyDrainAccount) bool {
	if currentID == "" {
		return false
	}
	var current WeeklyDrainAccount
	for _, account := range accounts {
		if account.ID == currentID {
			current = account
			break
		}
	}
	evidence := inspectWeeklyDrain(now, current)
	if !evidence.healthy || evidence.urgentReset.IsZero() {
		return false
	}
	for _, account := range accounts {
		if account.ID == currentID || (!account.EndsAt.IsZero() && !account.EndsAt.After(now)) {
			continue
		}
		if account.Priority > current.Priority {
			return false
		}
		if account.Priority != current.Priority {
			continue
		}
		other := inspectWeeklyDrain(now, account)
		if other.healthy && other.firstWeeklyReset.Before(evidence.firstWeeklyReset) {
			return false
		}
	}
	return true
}

type weeklyDrainEvidence struct {
	valid            bool
	healthy          bool
	minimum          float64
	firstWeeklyReset time.Time
	urgentReset      time.Time
	surplus          float64
}

func inspectWeeklyDrain(now time.Time, account WeeklyDrainAccount) weeklyDrainEvidence {
	if !account.EndsAt.IsZero() && !account.EndsAt.After(now) {
		return weeklyDrainEvidence{}
	}
	if account.ObservedAt.IsZero() || account.ObservedAt.After(now) || now.Sub(account.ObservedAt) > weeklyDrainFreshness || len(account.Windows) == 0 {
		return weeklyDrainEvidence{}
	}
	evidence := weeklyDrainEvidence{minimum: 1}
	accountWeekly := false
	for _, window := range account.Windows {
		if math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) || window.UsedPercent < 0 || window.UsedPercent > 100 ||
			window.LimitWindowSeconds <= 0 || !window.ResetAt.After(now) {
			return weeklyDrainEvidence{}
		}
		remaining := (100 - window.UsedPercent) / 100
		evidence.minimum = math.Min(evidence.minimum, remaining)
		if !isLongWindow(window) {
			continue
		}
		if window.Feature == "" {
			accountWeekly = true
		}
		deadline := window.ResetAt
		if !account.EndsAt.IsZero() && account.EndsAt.Before(deadline) {
			deadline = account.EndsAt
		}
		if evidence.firstWeeklyReset.IsZero() || deadline.Before(evidence.firstWeeklyReset) {
			evidence.firstWeeklyReset = deadline
		}
		until := deadline.Sub(now)
		surplus := remaining - until.Seconds()/float64(window.LimitWindowSeconds)
		// Use the same average-pace surplus as unpacedSurplus, deriving
		// time left from the absolute reset so it ages between refreshes.
		// A nearly untouched week with forty hours left needs several
		// short-window cycles to drain; a fixed last-hours gate starts late.
		if surplus <= 0 {
			continue
		}
		if evidence.urgentReset.IsZero() || deadline.Before(evidence.urgentReset) ||
			(deadline.Equal(evidence.urgentReset) && surplus > evidence.surplus) {
			evidence.urgentReset, evidence.surplus = deadline, surplus
		}
	}
	evidence.valid = accountWeekly
	evidence.healthy = accountWeekly && evidence.minimum > 0
	return evidence
}
