package selectacct

import (
	"math"
	"testing"
	"time"
)

func TestChooseWeeklyDrain(t *testing.T) {
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	makeAccount := func(id string, remaining, short float64, reset time.Duration) WeeklyDrainAccount {
		return WeeklyDrainAccount{ID: id, ObservedAt: now, Windows: []LimitWindow{
			{UsedPercent: 100 - remaining, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(reset)},
			{UsedPercent: 100 - short, LimitWindowSeconds: 5 * 60 * 60, ResetAt: now.Add(4 * time.Hour)},
		}}
	}
	tests := []struct {
		name    string
		current string
		change  func(*WeeklyDrainAccount, *WeeklyDrainAccount)
		want    string
	}{
		{name: "healthy sticky moves to expiring weekly quota", current: "later", want: "urgent"},
		{name: "new placement bypasses ordinary forty percent floor", want: "urgent"},
		{name: "short below forty percent remains eligible", current: "later", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].UsedPercent = 80 }, want: "urgent"},
		{name: "short exactly ten percent", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].UsedPercent = 90 }, want: "urgent"},
		{name: "short below ten percent", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].UsedPercent = 90.01 }, want: "urgent"},
		{name: "weekly below ten percent", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].UsedPercent = 95 }, want: "urgent"},
		{name: "weekly surplus below ten points", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].UsedPercent = 89 }, want: "urgent"},
		{name: "five percent weekly one hour before reset", current: "later", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 95
			a.Windows[0].ResetAt = now.Add(time.Hour)
		}, want: "urgent"},
		{name: "last positive short quota can receive drain", current: "later", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].UsedPercent = 99.9 }, want: "urgent"},
		{name: "last positive weekly quota before deadline", current: "later", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 99.9
			a.Windows[0].ResetAt = now.Add(time.Minute)
		}, want: "urgent"},
		{name: "positive weekly quota behind pace stays ordinary", current: "later", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].UsedPercent = 99.9 }},
		{name: "zero pace surplus stays ordinary", current: "later", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 50
			a.Windows[0].ResetAt = now.Add(84 * time.Hour)
		}},
		{name: "zero short quota cannot receive drain", current: "later", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].UsedPercent = 100 }},
		{name: "zero weekly quota cannot receive drain", current: "later", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].UsedPercent = 100 }},
		{name: "surplus six hours before reset", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].ResetAt = now.Add(6 * time.Hour) }, want: "urgent"},
		{name: "nearly untouched week forty hours before reset", current: "later", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 1
			a.Windows[0].ResetAt = now.Add(40 * time.Hour)
		}, want: "urgent"},
		{name: "fresh weekly quota does not cause migration", current: "later", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 1
			a.Windows[0].ResetAt = now.Add(7 * 24 * time.Hour)
		}},
		{name: "weekly quota on pace does not cause migration", current: "later", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 80
			a.Windows[0].ResetAt = now.Add(40 * time.Hour)
		}},
		{name: "stale destination", change: func(a, _ *WeeklyDrainAccount) { a.ObservedAt = now.Add(-2*time.Minute - time.Nanosecond) }},
		{name: "freshness exactly two minutes", change: func(a, _ *WeeklyDrainAccount) { a.ObservedAt = now.Add(-2 * time.Minute) }, want: "urgent"},
		{name: "unknown observation", change: func(a, _ *WeeklyDrainAccount) { a.ObservedAt = time.Time{} }},
		{name: "future observation", change: func(a, _ *WeeklyDrainAccount) { a.ObservedAt = now.Add(time.Second) }},
		{name: "stale current cannot justify migration", current: "later", change: func(_, a *WeeklyDrainAccount) { a.ObservedAt = now.Add(-time.Hour) }},
		{name: "missing current cannot justify migration", current: "missing"},
		{name: "higher manual priority prevents drain", current: "later", change: func(_, a *WeeklyDrainAccount) { a.Priority = 10 }},
		{name: "higher priority new placement prevents drain", change: func(_, a *WeeklyDrainAccount) { a.Priority = 10 }},
		{name: "higher urgent priority", current: "later", change: func(a, _ *WeeklyDrainAccount) { a.Priority = 10 }, want: "urgent"},
		{name: "earlier healthy current below ten percent retained", current: "later", change: func(_, a *WeeklyDrainAccount) {
			a.Windows[0].ResetAt = now.Add(time.Hour)
			a.Windows[0].UsedPercent = 99
		}},
		{name: "equal current reset retained", current: "later", change: func(_, a *WeeklyDrainAccount) { a.Windows[0].ResetAt = now.Add(3 * time.Hour) }},
		{name: "current short exhausted does not retain", current: "later", change: func(_, a *WeeklyDrainAccount) {
			a.Windows[0].ResetAt = now.Add(time.Hour)
			a.Windows[1].UsedPercent = 100
		}, want: "urgent"},
		{name: "current weekly exhausted does not retain", current: "later", change: func(_, a *WeeklyDrainAccount) {
			a.Windows[0].ResetAt = now.Add(time.Hour)
			a.Windows[0].UsedPercent = 100
		}, want: "urgent"},
		{name: "unknown weekly absolute reset", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].ResetAt = time.Time{}
			a.Windows[0].ResetAfterSeconds = 3600
		}},
		{name: "weekly reset reached", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].ResetAt = now }},
		{name: "unknown short reset", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].ResetAt = time.Time{} }},
		{name: "no account wide weekly evidence", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].Feature = "claudeopus" }},
		{name: "unknown window length", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].LimitWindowSeconds = 0 }},
		{name: "model exhausted", change: func(a, _ *WeeklyDrainAccount) {
			w := a.Windows[0]
			w.Feature = "claudeopus"
			w.UsedPercent = 100
			a.Windows = append(a.Windows, w)
		}},
		{name: "positive model quota below former destination floor", change: func(a, _ *WeeklyDrainAccount) {
			w := a.Windows[0]
			w.Feature = "claudeopus"
			w.UsedPercent = 95
			a.Windows = append(a.Windows, w)
		}, want: "urgent"},
		{name: "healthy model quota does not hide account surplus", change: func(a, _ *WeeklyDrainAccount) {
			w := a.Windows[0]
			w.Feature = "claudeopus"
			w.UsedPercent = 10
			w.ResetAt = now.Add(6 * 24 * time.Hour)
			a.Windows = append(a.Windows, w)
		}, want: "urgent"},
		{name: "no windows", change: func(a, _ *WeeklyDrainAccount) { a.Windows = nil }},
		{name: "empty account id", change: func(a, _ *WeeklyDrainAccount) { a.ID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			urgent := makeAccount("urgent", 34, 48, 3*time.Hour)
			later := makeAccount("later", 99, 99, 7*24*time.Hour)
			if tt.change != nil {
				tt.change(&urgent, &later)
			}
			got, ok := ChooseWeeklyDrain(now, tt.current, []WeeklyDrainAccount{later, urgent})
			if got.AccountID != tt.want || ok != (tt.want != "") {
				t.Fatalf("choice = %+v, %v; want %q", got, ok, tt.want)
			}
			if ok && !got.ResetAt.Equal(urgent.Windows[0].ResetAt) {
				t.Fatalf("reset = %s; want %s", got.ResetAt, urgent.Windows[0].ResetAt)
			}
		})
	}
	for _, used := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.01, 100.01} {
		urgent := makeAccount("urgent", 34, 48, 3*time.Hour)
		urgent.Windows[0].UsedPercent = used
		if got, ok := ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{urgent}); ok {
			t.Errorf("invalid utilization %v produced %+v", used, got)
		}
	}
	if got, ok := ChooseWeeklyDrain(now, "", nil); ok || got != (WeeklyDrainChoice{}) {
		t.Fatalf("empty pool produced %+v, %v", got, ok)
	}
}

func TestWeeklyDrainOrderingAndReentry(t *testing.T) {
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	makeAccount := func(id string, used float64, reset time.Duration) WeeklyDrainAccount {
		return WeeklyDrainAccount{ID: id, ObservedAt: now, Windows: []LimitWindow{
			{UsedPercent: used, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(reset)},
			{UsedPercent: 52, LimitWindowSeconds: 5 * 60 * 60, ResetAt: now.Add(4 * time.Hour)},
		}}
	}
	a, b := makeAccount("a", 66, 3*time.Hour), makeAccount("b", 50, 4*time.Hour)
	choice, ok := ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{b, a})
	if !ok || choice.AccountID != "a" {
		t.Fatalf("earliest reset should win: %+v, %v", choice, ok)
	}
	b.Windows[0].ResetAt = a.Windows[0].ResetAt
	choice, ok = ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{a, b})
	if !ok || choice.AccountID != "b" {
		t.Fatalf("larger surplus should break reset tie: %+v, %v", choice, ok)
	}
	b.Windows[0].UsedPercent = a.Windows[0].UsedPercent
	for _, pool := range [][]WeeklyDrainAccount{{a, b}, {b, a}} {
		choice, ok = ChooseWeeklyDrain(now, "", pool)
		if !ok || choice.AccountID != "a" {
			t.Fatalf("id should deterministically break tie: %+v, %v", choice, ok)
		}
	}
	// A successful move need not create policy state: the destination's
	// earlier reset prevents a move back even after its surplus is consumed.
	a.Windows[0].UsedPercent = 99
	if choice, ok := ChooseWeeklyDrain(now, "a", []WeeklyDrainAccount{b, a}); ok {
		t.Fatalf("healthy current should stay after drain: %+v", choice)
	}
	a.Windows[1].UsedPercent = 100
	if choice, ok := ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{a}); ok {
		t.Fatalf("short-window exhaustion must stop drain: %+v", choice)
	}
	// A fresh short-window reset permits the same weekly quota to re-enter.
	a.Windows[0].UsedPercent = 66
	a.Windows[1].UsedPercent = 0
	a.Windows[1].ResetAt = now.Add(5 * time.Hour)
	if choice, ok := ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{a}); !ok || choice.AccountID != "a" {
		t.Fatalf("short-window recovery should re-enter: %+v, %v", choice, ok)
	}
	a.ObservedAt = now.Add(3 * time.Hour)
	a.Windows[1].ResetAt = now.Add(8 * time.Hour)
	if choice, ok := ChooseWeeklyDrain(now.Add(3*time.Hour), "", []WeeklyDrainAccount{a}); ok {
		t.Fatalf("old weekly reset must stop drain: %+v", choice)
	}
	a.Windows[0].ResetAt = now.Add(7*24*time.Hour + 3*time.Hour)
	if choice, ok := ChooseWeeklyDrain(now.Add(3*time.Hour), "", []WeeklyDrainAccount{a}); ok {
		t.Fatalf("new weekly cycle must stop urgent drain: %+v", choice)
	}
}

func TestWeeklyDrainAcrossShortAndWeeklyResets(t *testing.T) {
	start := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	for cycle := 0; cycle < 3; cycle++ {
		reset := start.Add(40*time.Hour + time.Duration(cycle)*week)
		for shortCycle := 0; shortCycle < 8; shortCycle++ {
			now := reset.Add(-40*time.Hour + time.Duration(shortCycle)*5*time.Hour)
			pool := []WeeklyDrainAccount{
				{ID: "drain", ObservedAt: now, Windows: []LimitWindow{
					{UsedPercent: 1 + float64(shortCycle)*10, LimitWindowSeconds: int64(week.Seconds()), ResetAt: reset},
					{UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60, ResetAt: now.Add(5 * time.Hour)},
				}},
				{ID: "fresh", ObservedAt: now, Windows: []LimitWindow{
					{UsedPercent: 1, LimitWindowSeconds: int64(week.Seconds()), ResetAt: now.Add(week)},
				}},
			}
			choice, ok := ChooseWeeklyDrain(now, "fresh", pool)
			if !ok || choice.AccountID != "drain" || !choice.ResetAt.Equal(reset) {
				t.Fatalf("week %d, short cycle %d: recovered quota should drain: %+v, %v", cycle, shortCycle, choice, ok)
			}
			// A spent short window stops draining until a fresh snapshot
			// confirms the next short-window reset restored capacity.
			pool[0].Windows[1].UsedPercent = 100
			if choice, ok := ChooseWeeklyDrain(now, "fresh", pool); ok {
				t.Fatalf("week %d, short cycle %d: exhausted short quota selected: %+v", cycle, shortCycle, choice)
			}
		}
		fresh := WeeklyDrainAccount{ID: "drain", ObservedAt: reset, Windows: []LimitWindow{
			{UsedPercent: 1, LimitWindowSeconds: int64(week.Seconds()), ResetAt: reset.Add(week)},
			{UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60, ResetAt: reset.Add(5 * time.Hour)},
		}}
		if choice, ok := ChooseWeeklyDrain(reset, "", []WeeklyDrainAccount{fresh}); ok {
			t.Fatalf("week %d: fresh weekly reset should end discretionary drain: %+v", cycle, choice)
		}
	}
}

func TestWeeklyDrainAccountEnd(t *testing.T) {
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	makeAccount := func(id string, reset, end time.Duration) WeeklyDrainAccount {
		acct := WeeklyDrainAccount{ID: id, ObservedAt: now, Windows: []LimitWindow{
			{UsedPercent: 1, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(reset)},
			{UsedPercent: 52, LimitWindowSeconds: 5 * 60 * 60, ResetAt: now.Add(4 * time.Hour)},
		}}
		if end != 0 {
			acct.EndsAt = now.Add(end)
		}
		return acct
	}
	for _, tt := range []struct {
		name     string
		reset    time.Duration
		end      time.Duration
		wantLoss time.Duration
	}{
		{"end before weekly reset", 7 * 24 * time.Hour, 40 * time.Hour, 40 * time.Hour},
		{"weekly reset before end", 3 * time.Hour, 40 * time.Hour, 3 * time.Hour},
		{"end equals reset", 3 * time.Hour, 3 * time.Hour, 3 * time.Hour},
		{"unknown end preserves weekly reset", 3 * time.Hour, 0, 3 * time.Hour},
		{"past end excluded", 3 * time.Hour, -time.Nanosecond, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			acct := makeAccount("ending", tt.reset, tt.end)
			choice, ok := ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{acct})
			if tt.wantLoss == 0 {
				if ok {
					t.Fatalf("ended account chosen: %+v", choice)
				}
				return
			}
			if !ok || choice.AccountID != acct.ID || !choice.ResetAt.Equal(now.Add(tt.wantLoss)) {
				t.Fatalf("choice = %+v, %v; want deadline %s", choice, ok, now.Add(tt.wantLoss))
			}
		})
	}
	ending := makeAccount("ending", 7*24*time.Hour, 40*time.Hour)
	weekly := makeAccount("weekly", 48*time.Hour, 0)
	choice, ok := ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{weekly, ending})
	if !ok || choice.AccountID != "ending" {
		t.Fatalf("earlier account end should beat later reset: %+v, %v", choice, ok)
	}
	// The current account's earlier end retains its prompt cache even when
	// another account's weekly reset is earlier than the current weekly reset.
	if choice, ok := ChooseWeeklyDrain(now, "ending", []WeeklyDrainAccount{weekly, ending}); ok {
		t.Fatalf("earlier-ending current account should stay: %+v", choice)
	}
	if choice, ok := ChooseWeeklyDrain(now, "weekly", []WeeklyDrainAccount{weekly, ending}); !ok || choice.AccountID != "ending" {
		t.Fatalf("later current should move to earlier end: %+v, %v", choice, ok)
	}
	ending.EndsAt = now
	ending.Priority = 10
	choice, ok = ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{ending, weekly})
	if !ok || choice.AccountID != "weekly" {
		t.Fatalf("end expiry must remove higher priority account: %+v, %v", choice, ok)
	}
	// An end date never substitutes for current authoritative quota evidence.
	ending.EndsAt = now.Add(time.Hour)
	ending.Windows[0].ResetAt = time.Time{}
	if choice, ok := ChooseWeeklyDrain(now, "", []WeeklyDrainAccount{ending}); ok {
		t.Fatalf("known end with unknown quota must not route: %+v", choice)
	}
}

func TestPreferWeeklyDrainQueue(t *testing.T) {
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	makeAccount := func(id string, remaining float64, reset time.Duration) WeeklyDrainAccount {
		return WeeklyDrainAccount{ID: id, ObservedAt: now, Windows: []LimitWindow{
			{UsedPercent: 100 - remaining, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(reset)},
			{UsedPercent: 52, LimitWindowSeconds: 5 * 60 * 60, ResetAt: now.Add(4 * time.Hour)},
		}}
	}
	for _, tt := range []struct {
		name   string
		change func(*WeeklyDrainAccount, *WeeklyDrainAccount)
		want   bool
	}{
		{name: "expiring weekly quota below forty percent", want: true},
		{name: "short quota below ten percent stays warm", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].UsedPercent = 95 }, want: true},
		{name: "weekly quota below ten percent stays warm", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].UsedPercent = 95 }, want: true},
		{name: "last positive weekly quota ahead of pace", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 99.9
			a.Windows[0].ResetAt = now.Add(time.Minute)
		}, want: true},
		{name: "positive short quota below ordinary sticky floor", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].UsedPercent = 99.9 }, want: true},
		{name: "short exhausted", change: func(a, _ *WeeklyDrainAccount) { a.Windows[1].UsedPercent = 100 }},
		{name: "weekly exhausted", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].UsedPercent = 100 }},
		{name: "model exhausted", change: func(a, _ *WeeklyDrainAccount) {
			w := a.Windows[0]
			w.Feature = "claudehaiku"
			w.UsedPercent = 100
			a.Windows = append(a.Windows, w)
		}},
		{name: "stale current", change: func(a, _ *WeeklyDrainAccount) { a.ObservedAt = now.Add(-time.Hour) }},
		{name: "unknown current evidence", change: func(a, _ *WeeklyDrainAccount) { a.ObservedAt = time.Time{} }},
		{name: "ended current", change: func(a, _ *WeeklyDrainAccount) { a.EndsAt = now }},
		{name: "current weekly reset reached", change: func(a, _ *WeeklyDrainAccount) { a.Windows[0].ResetAt = now }},
		{name: "earlier alternative", change: func(_, a *WeeklyDrainAccount) { a.Windows[0].ResetAt = now.Add(time.Hour) }},
		{name: "equal deadline keeps warm cache", change: func(_, a *WeeklyDrainAccount) { a.Windows[0].ResetAt = now.Add(3 * time.Hour) }, want: true},
		{name: "higher manual priority alternative", change: func(_, a *WeeklyDrainAccount) { a.Priority = 10 }},
		{name: "higher current priority", change: func(a, _ *WeeklyDrainAccount) { a.Priority = 10 }, want: true},
		{name: "earlier lower priority alternative", change: func(_, a *WeeklyDrainAccount) { a.Windows[0].ResetAt = now.Add(time.Hour); a.Priority = -10 }, want: true},
		{name: "stale earlier alternative", change: func(_, a *WeeklyDrainAccount) {
			a.Windows[0].ResetAt = now.Add(time.Hour)
			a.ObservedAt = now.Add(-time.Hour)
		}, want: true},
		{name: "exhausted earlier alternative", change: func(_, a *WeeklyDrainAccount) {
			a.Windows[0].ResetAt = now.Add(time.Hour)
			a.Windows[1].UsedPercent = 100
		}, want: true},
		{name: "expired higher priority alternative", change: func(_, a *WeeklyDrainAccount) { a.Priority = 10; a.EndsAt = now }, want: true},
		{name: "on pace current unchanged", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 95
			a.Windows[0].ResetAt = now.Add(12 * time.Hour)
		}},
		{name: "fresh weekly current unchanged", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 1
			a.Windows[0].ResetAt = now.Add(7 * 24 * time.Hour)
		}},
		{name: "known end brings fresh weekly quota forward", change: func(a, _ *WeeklyDrainAccount) {
			a.Windows[0].UsedPercent = 1
			a.Windows[0].ResetAt = now.Add(7 * 24 * time.Hour)
			a.EndsAt = now.Add(40 * time.Hour)
		}, want: true},
		{name: "earlier known end alternative", change: func(_, a *WeeklyDrainAccount) { a.EndsAt = now.Add(time.Hour) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			current := makeAccount("current", 34, 3*time.Hour)
			other := makeAccount("other", 99, 7*24*time.Hour)
			if tt.change != nil {
				tt.change(&current, &other)
			}
			if got := PreferWeeklyDrainQueue(now, "current", []WeeklyDrainAccount{other, current}); got != tt.want {
				t.Fatalf("PreferWeeklyDrainQueue = %v; want %v", got, tt.want)
			}
		})
	}
	if PreferWeeklyDrainQueue(now, "", nil) || PreferWeeklyDrainQueue(now, "missing", nil) {
		t.Fatal("missing session/account should retain ordinary spill behavior")
	}
}

func TestWeeklyDrainLowWeeklyQuotaReentersAfterFiveHourReset(t *testing.T) {
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	pool := []WeeklyDrainAccount{
		{ID: "drain", ObservedAt: now, Windows: []LimitWindow{
			{UsedPercent: 95, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(6 * time.Hour)},
			{UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60, ResetAt: now.Add(5 * time.Hour)},
		}},
		{ID: "current", ObservedAt: now, Windows: []LimitWindow{
			{UsedPercent: 1, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: now.Add(7 * 24 * time.Hour)},
		}},
	}
	if choice, ok := ChooseWeeklyDrain(now, "current", pool); ok {
		t.Fatalf("short exhaustion should prevent entry: %+v", choice)
	}
	afterReset := now.Add(5 * time.Hour)
	pool[0].ObservedAt = afterReset
	pool[1].ObservedAt = afterReset
	pool[0].Windows[1].UsedPercent = 0
	pool[0].Windows[1].ResetAt = afterReset.Add(5 * time.Hour)
	choice, ok := ChooseWeeklyDrain(afterReset, "current", pool)
	if !ok || choice.AccountID != "drain" || !choice.ResetAt.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("five percent weekly quota should re-enter with one hour left: %+v, %v", choice, ok)
	}
}
