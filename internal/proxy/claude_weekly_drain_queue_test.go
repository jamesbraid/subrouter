package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestClaudeWeeklyDrainQueuePrecedesProactiveSpill(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func(*Server)
		wantSpill    bool
		newPlacement bool
	}{
		{name: "haiku keeps urgent warm account"},
		{name: "cold placement retains ordinary proactive spill", newPlacement: true, wantSpill: true},
		{name: "warm account below ten percent", change: func(s *Server) {
			e := s.AccountRef.usageWindows["expiring\x00claude"]
			e.windows[0].UsedPercent = 99
			e.windows[1].UsedPercent = 95
			e.windows[1].ResetAt = time.Now().Add(time.Hour)
			s.AccountRef.usageWindows["expiring\x00claude"] = e
		}},
		{name: "stale evidence permits ordinary spill", wantSpill: true, change: func(s *Server) {
			e := s.AccountRef.usageWindows["expiring\x00claude"]
			e.at = time.Now().Add(-time.Hour)
			e.orderAt = e.at
			s.AccountRef.usageWindows["expiring\x00claude"] = e
		}},
		{name: "actual exhaustion permits fallback", wantSpill: true, change: func(s *Server) {
			s.SchedulerRef = selectacct.NewSchedulerRef(s.Scheduler)
			s.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "expiring", "claude-haiku-5-5", time.Now().Add(time.Hour))
		}},
		{name: "real admission pause permits fallback", wantSpill: true, change: func(s *Server) {
			s.admission.mu.Lock()
			s.admission.bucketLocked(admissionKeyFor(accounts.ProviderClaude, "expiring", "claude-haiku-5-5")).pausedTil = time.Now().Add(time.Hour)
			s.admission.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			_, a := weeklyDrainRequest(t, s, "fanout")
			defer a.finishWeeklyDrain()
			a.account = s.Accounts[1]
			a.poolModel = "claude-haiku-5-5"
			s.admission.limit = 1
			s.admission.spillQueue = 1
			key := admissionKeyFor(accounts.ProviderClaude, "expiring", a.poolModel)
			s.admission.mu.Lock()
			bucket := s.admission.bucketLocked(key)
			bucket.active = 1
			bucket.waiters = append(bucket.waiters, &admissionWaiter{ready: make(chan struct{})})
			s.admission.mu.Unlock()
			if tc.change != nil {
				tc.change(s)
			}
			layer := usageLimitRetryTransport{server: s, attempt: a, provider: accounts.ProviderClaude, agent: "claude", session: "fanout", method: http.MethodPost, path: "/v1/messages", poolModel: a.poolModel, weeklyDrainNewPlacement: tc.newPlacement}
			_, lease, spilled := layer.admissionSpill(context.Background(), "expiring", nil)
			if lease != nil {
				lease.release()
			}
			if spilled != tc.wantSpill {
				t.Fatalf("proactive spill=%v want %v; urgent warm quota should queue before spending a later account", spilled, tc.wantSpill)
			}
		})
	}
}

func TestClaudeWeeklyDrainHandlerQueuesButStillOverflowsWhenFull(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "queues_on_expiring_quota"
		if full {
			name = "full_queue_still_fails_over"
		}
		t.Run(name, func(t *testing.T) {
			s := weeklyDrainFixture(t)
			s.admission.limit = 1
			s.admission.spillQueue = 1
			s.admission.queueCap = 4
			if full {
				s.admission.queueCap = 1
			}
			calls := make(chan string, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls <- r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"type":"message"}`))
			}))
			defer upstream.Close()
			s.ClaudeUpstream = mustParseURL(t, upstream.URL)
			handler := s.Handler()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			key := admissionKeyFor(accounts.ProviderClaude, "expiring", "claude-haiku-5-5")
			held, ok := s.admission.tryAcquire(key, admissionSessionKey{agent: "claude", session: "holder"})
			if !ok {
				t.Fatal("cannot reserve initial slot")
			}
			defer held.release()
			done := make(chan int, 2)
			send := func(id string) {
				if _, err := s.Sessions.Put("claude", id, "expiring", ""); err != nil {
					t.Fatal(err)
				}
				go func() {
					r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-haiku-5-5","messages":[]}`)).WithContext(ctx)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("X-Subrouter-Agent", "claude")
					r.Header.Set("X-Subrouter-Session", id)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					done <- w.Code
				}()
			}
			waitQueued := func(n int) {
				t.Helper()
				deadline := time.Now().Add(2 * time.Second)
				for {
					s.admission.mu.Lock()
					queued := len(s.admission.bucketLocked(key).waiters)
					s.admission.mu.Unlock()
					if queued == n {
						return
					}
					select {
					case token := <-calls:
						t.Fatalf("unexpected proactive spill before releasing warm slot: %s", token)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatalf("queued=%d want %d", queued, n)
					}
					time.Sleep(time.Millisecond)
				}
			}
			send("first")
			waitQueued(1)
			send("second")
			if full {
				select {
				case token := <-calls:
					if token == "Bearer expiring-token" {
						t.Fatal("full source queue unexpectedly granted slot")
					}
				case <-ctx.Done():
					t.Fatal("full queue did not fall back")
				}
			} else {
				waitQueued(2)
			}
			held.release()
			for range 2 {
				select {
				case code := <-done:
					if code != 200 {
						t.Fatalf("status=%d", code)
					}
				case <-ctx.Done():
					t.Fatal("queued request did not finish")
				}
			}
			want := 2
			if full {
				want = 1
			}
			for range want {
				select {
				case token := <-calls:
					if token != "Bearer expiring-token" {
						t.Fatalf("queued request served elsewhere: %s", token)
					}
				case <-ctx.Done():
					t.Fatal("missing queued request")
				}
			}
		})
	}
}
