package main

import (
	"strings"
	"testing"
)

func TestServeRejectsInvalidAdmissionLimitBeforeStartup(t *testing.T) {
	for _, raw := range []string{"claude/haiku=0", "claude/haiku=65", "haiku=16"} {
		err := serve([]string{"--admission-limit", raw})
		if err == nil || !strings.Contains(err.Error(), "admission limit") {
			t.Fatalf("serve invalid config%q: %v", raw, err)
		}
	}
}
