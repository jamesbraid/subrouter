package proxy

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

const maxAdmissionLimit = 64

// AdmissionLimit overrides physical request concurrency for one provider/model.
// A Claude family name applies to its versioned models too.
type AdmissionLimit struct {
	Provider accounts.Provider `json:"provider"`
	Model    string            `json:"model"`
	Limit    int               `json:"limit"`
}

type admissionLimitKey struct {
	provider accounts.Provider
	model    string
}

// ParseAdmissionLimits parses repeatable provider/model=limit serve flags.
func ParseAdmissionLimits(values []string) ([]AdmissionLimit, error) {
	result := make([]AdmissionLimit, 0, len(values))
	seen := make(map[admissionLimitKey]bool)
	for _, value := range values {
		target, raw, ok := strings.Cut(value, "=")
		provider, model, scoped := strings.Cut(target, "/")
		limit, err := strconv.Atoi(raw)
		provider = strings.TrimSpace(strings.ToLower(provider))
		model = strings.TrimSpace(model)
		if !ok || !scoped || provider == "" || model == "" || strings.ContainsAny(provider+model, " /\t\r\n=") || err != nil || limit < 1 || limit > maxAdmissionLimit {
			return nil, fmt.Errorf("admission limit %q must be provider/model=1..%d", value, maxAdmissionLimit)
		}
		entry := AdmissionLimit{Provider: accounts.Provider(provider), Model: admissionLimitModel(accounts.Provider(provider), model), Limit: limit}
		key := admissionLimitKey{entry.Provider, entry.Model}
		if seen[key] {
			return nil, fmt.Errorf("duplicate admission limit for %s/%s", entry.Provider, entry.Model)
		}
		seen[key] = true
		result = append(result, entry)
	}
	return result, nil
}

func claudeHaikuAdmissionModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "haiku" || model == "claude-haiku" || strings.HasPrefix(model, "claude-haiku-")
}

func admissionLimitModel(provider accounts.Provider, model string) string {
	if provider == accounts.ProviderClaude {
		if claudeHaikuAdmissionModel(model) {
			return "claude-haiku"
		}
		model = claudePoolModel(model)
	}
	return selectacct.ModelKey(model)
}

func newAdmissionControlWithLimits(limits []AdmissionLimit) *admissionControl {
	control := newAdmissionControl()
	control.modelLimits = make(map[admissionLimitKey]int, len(limits))
	for _, entry := range limits {
		// Programmatic callers get the same bounded behavior as the serve flags.
		if entry.Provider == "" || entry.Model == "" || entry.Limit < 1 || entry.Limit > maxAdmissionLimit {
			continue
		}
		control.modelLimits[admissionLimitKey{entry.Provider, admissionLimitModel(entry.Provider, entry.Model)}] = entry.Limit
	}
	return control
}

func (a *admissionControl) configuredLimitLocked(bucket *admissionBucket) int {
	if bucket.limit > 0 {
		return bucket.limit
	}
	return a.limit
}
