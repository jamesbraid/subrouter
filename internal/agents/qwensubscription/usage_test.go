package qwensubscription

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type quotaFixture struct {
	t               *testing.T
	mu              sync.Mutex
	generation      int
	refreshes       int
	wrongOwner      bool
	webID           string
	omitWebID       bool
	profileFailure  bool
	quotaFailure    bool
	refreshRedirect bool
	refreshFailure  bool
	quotaBody       string
	quotaCalls      int
	allowOldAccess  bool
	cookieReplays   int
	familyRevoked   bool
	profileCalls    int
	failProfileAt   int
}

func (f *quotaFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	for _, header := range []string{"Version", "source", "Timezone", "X-Request-Id", "Content-Type"} {
		if r.Header.Get(header) == "" {
			f.t.Errorf("missing required web header %s", header)
		}
	}
	status, body := http.StatusOK, ""
	headers := make(http.Header)
	token := func() string { return "web-token-" + string(rune('0'+f.generation)) }
	cookie := func() string { return "refresh-" + string(rune('0'+f.generation)) }
	switch r.URL.String() {
	case quotaRefreshURL:
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
			f.t.Error("session refresh must use cookies without Authorization")
		}
		got, err := r.Cookie("refresh_token")
		if err != nil || got.Value != cookie() || f.familyRevoked {
			f.cookieReplays++
			f.familyRevoked = true
			status = http.StatusUnauthorized
			body = `{"success":false,"errorCode":"NOT_LOGIN"}`
			break
		}
		if f.refreshRedirect {
			status = http.StatusTemporaryRedirect
			headers.Set("Location", "https://untrusted.example.test/refresh")
			body = `{}`
			break
		}
		if f.refreshFailure {
			status = http.StatusServiceUnavailable
			body = `{}`
			break
		}
		f.generation++
		f.refreshes++
		headers.Add("Set-Cookie", (&http.Cookie{Name: "refresh_token", Value: cookie(), Domain: ".qwen.ai", Path: "/", Secure: true, HttpOnly: true}).String())
		body = `{"success":true,"data":{"access_token":"` + token() + `","expires_in":900}}`
	case webProfileURL:
		f.profileCalls++
		if f.familyRevoked {
			status = http.StatusUnauthorized
			body = `{"success":false,"errorCode":"NOT_LOGIN"}`
			break
		}
		if r.Header.Get("Authorization") != "Bearer "+token() && !(f.allowOldAccess && r.Header.Get("Authorization") == "Bearer web-token-0") {
			f.t.Error("profile received the wrong bearer")
		}
		if f.profileFailure || f.profileCalls == f.failProfileAt {
			status = http.StatusServiceUnavailable
			body = `{"secret":"must-not-leak"}`
			break
		}
		email := "verified@example.test"
		if f.wrongOwner {
			email = "other@example.test"
		}
		id := f.webID
		if id == "" && !f.omitWebID {
			id = "web-account"
		}
		body = `{"success":true,"data":{"id":"` + id + `","email":"` + email + `"}}`
	default:
		f.quotaCalls++
		if r.URL.Host != "sg-entitlement.qwen.com" || r.URL.Path != "/api/entitlement/agent/usage" {
			f.t.Fatalf("unexpected session endpoint: %s", r.URL.Host+r.URL.Path)
		}
		if r.Header.Get("Cookie") != "" {
			f.t.Error("session cookies leaked to entitlement")
		}
		if r.Header.Get("Authorization") != "Bearer "+token() {
			f.t.Error("quota received the wrong bearer")
		}
		if r.URL.Query().Get("biz_id") != "ai_qwen" || r.URL.Query().Get("pr") != "qwen-i18n" {
			f.t.Error("missing fixed quota selectors")
		}
		if f.quotaFailure {
			status = http.StatusServiceUnavailable
			body = `{"secret":"must-not-leak"}`
			break
		}
		body = f.quotaBody
		if body == "" {
			encoded, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"remainingPercent": 62.5, "resetAt": time.Now().Add(3 * 24 * time.Hour).UnixMilli()}})
			body = string(encoded)
		}
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestQwenQuotaDuplicateEnrollmentKeepsOwnedCookieChain(t *testing.T) {
	for _, stage := range []string{"ready", "profile", "quota"} {
		t.Run(stage, func(t *testing.T) {
			store, f, client, credential := quotaFixtureStore(t)
			f.allowOldAccess = true
			f.quotaFailure = stage == "quota"
			if stage == "profile" {
				f.failProfileAt = 2
			}
			submitted := initialQuotaSession()
			err := store.EnrollQuota(t.Context(), client, "work", submitted)
			if stage != "ready" && err == nil || stage == "ready" && err != nil {
				t.Fatal("unexpected initial enrollment result", err)
			}
			f.quotaFailure = false
			if err := store.EnrollQuota(t.Context(), client, "work", submitted); err != nil {
				t.Error("identical enrollment retry failed", err)
			}
			if f.cookieReplays != 0 || f.refreshes != 1 {
				t.Errorf("duplicate redeemed original cookie: replays=%d rotations=%d", f.cookieReplays, f.refreshes)
			}
			if _, err := store.quotaWindows(t.Context(), client, "work", credential); err != nil {
				t.Error("duplicate damaged owned session", err)
			}
			saved, err := store.readQuota("work")
			if err != nil || saved.VerificationPending || saved.AccessToken != "web-token-1" {
				t.Error("duplicate did not preserve and verify current session", err)
			}
		})
	}
}

func TestQwenQuotaOriginalGrantBindingSurvivesMetadataChanges(t *testing.T) {
	store, f, client, credential := quotaFixtureStore(t)
	submitted := initialQuotaSession()
	if err := store.EnrollQuota(t.Context(), client, "work", submitted); err != nil {
		t.Fatal(err)
	}
	submitted.AccessToken = "expired-original-bearer"
	submitted.ExpiresAt = time.Now().Add(-time.Hour)
	submitted.Subject = "client-claimed-subject"
	submitted.Email = "untrusted@example.test"
	submitted.VerificationPending = true
	submitted.RenewalPending = true
	submitted.Cookies[0].Domain = "qwen.ai"
	submitted.Cookies[0].Expires = time.Now().Add(-time.Hour)
	if err := store.EnrollQuota(t.Context(), client, "work", submitted); err != nil {
		t.Fatal("metadata turned retry into new grant", err)
	}
	saved, err := store.readQuota("work")
	if err != nil || saved.Subject != credential.Subject || saved.Email != credential.Email || saved.VerificationPending || f.cookieReplays != 0 || f.refreshes != 1 {
		t.Fatal("client metadata replaced owned state", err)
	}
}

func TestQwenQuotaOriginalGrantCannotCrossInferenceReplacement(t *testing.T) {
	store, f, client, credential := quotaFixtureStore(t)
	f.allowOldAccess = true
	submitted := initialQuotaSession()
	if err := store.EnrollQuota(t.Context(), client, "work", submitted); err != nil {
		t.Fatal(err)
	}
	credential.Subject = "replacement-membership"
	// Leave the old sidecar in place to exercise the binding check even when
	// account recovery restores an older quota file beside a new enrollment.
	if _, err := store.write("work", credential); err != nil {
		t.Fatal(err)
	}
	if err := store.EnrollQuota(t.Context(), client, "work", submitted); err == nil {
		t.Fatal("original grant crossed replacement subject")
	}
	if f.cookieReplays != 0 || f.refreshes != 1 {
		t.Fatal("stale membership redeemed a spent cookie")
	}
	newGrant := f.issueGrant()
	if err := store.EnrollQuota(t.Context(), client, "work", newGrant); err != nil {
		t.Fatal("new grant could not enroll replacement", err)
	}
	if err := store.EnrollQuota(t.Context(), client, "work", newGrant); err != nil {
		t.Fatal("new grant retry was not idempotent", err)
	}
	saved, err := store.readQuota("work")
	if err != nil || saved.Subject != credential.Subject || f.cookieReplays != 0 || f.refreshes != 2 {
		t.Fatal("replacement grant binding incorrect", err)
	}
}

func TestQwenQuotaDeferredOwnerCheckDoesNotPromoteWrongAccount(t *testing.T) {
	store, f, client, _ := quotaFixtureStore(t)
	f.wrongOwner = true
	grant := f.issueGrant()
	if err := store.EnrollQuota(t.Context(), client, "work", grant); err == nil {
		t.Fatal("server accepted another web owner")
	}
	if _, err := store.readQuota("work"); !errors.Is(err, ErrQuotaLoginRequired) {
		t.Fatal("wrong owner became serving state", err)
	}
}

func quotaFixtureStore(t *testing.T) (Store, *quotaFixture, *http.Client, CredentialInfo) {
	t.Helper()
	store := Store{Dir: t.TempDir()}
	credential := testCredential()
	if _, err := store.SaveManagedCredential("work", credential); err != nil {
		t.Fatal(err)
	}
	fixture := &quotaFixture{t: t}
	client := &http.Client{Transport: fixture}
	return store, fixture, client, credential
}

func initialQuotaSession() QuotaSession {
	return QuotaSession{Version: 2, AccessToken: "web-token-0", ExpiresAt: time.Now().Add(15 * time.Minute), Cookies: []QuotaCookie{{Name: "refresh_token", Value: "refresh-0", Domain: ".qwen.ai", Path: "/", Secure: true, HTTPOnly: true}}}
}

func TestQwenQuotaHeadlessRenewal(t *testing.T) {
	store, f, client, credential := quotaFixtureStore(t)
	session := f.issueGrant()
	if err := store.EnrollQuota(t.Context(), client, "work", session); err != nil {
		t.Fatal(err)
	}
	saved, err := store.readQuota("work")
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "web-token-1" || saved.VerificationPending || saved.Subject != credential.Subject {
		t.Fatal("server did not independently take session ownership")
	}
	path, _ := store.quotaPath("work")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("session file is not private")
	}
	saved.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.writeQuota("work", saved); err != nil {
		t.Fatal(err)
	}
	restarted := Store{Dir: store.Dir}
	windows, err := restarted.quotaWindows(t.Context(), client, "work", credential)
	if err != nil || len(windows) != 1 || windows[0].UsedPercent != 37.5 || windows[0].Name != "weekly" {
		t.Fatal("headless renewal lost weekly quota", windows, err)
	}
	if f.refreshes != 2 {
		t.Fatal("expired access did not renew exactly once")
	}
	inference, _, _ := store.read("work")
	if inference.AccessToken != credential.AccessToken || inference.RefreshToken != credential.RefreshToken {
		t.Fatal("quota renewal changed inference credentials")
	}
}

func TestQwenQuotaPreservesRotationBeforeLaterFailure(t *testing.T) {
	for _, stage := range []string{"profile", "quota"} {
		t.Run(stage, func(t *testing.T) {
			store, f, client, credential := quotaFixtureStore(t)
			if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err != nil {
				t.Fatal(err)
			}
			session, _ := store.readQuota("work")
			session.ExpiresAt = time.Now().Add(-time.Minute)
			_ = store.writeQuota("work", session)
			f.profileFailure = stage == "profile"
			f.quotaFailure = stage == "quota"
			if _, err := store.quotaWindows(t.Context(), client, "work", credential); err == nil || strings.Contains(err.Error(), "must-not-leak") {
				t.Fatal("later failure hidden or leaked", err)
			}
			session, err := store.readQuota("work")
			if err != nil || session.AccessToken != "web-token-2" || session.Cookies[0].Value != "refresh-2" || !session.VerificationPending {
				t.Fatalf("rotation not retained: err=%v access_match=%t cookie_match=%t pending=%t refreshes=%d", err, session.AccessToken == "web-token-2", session.Cookies[0].Value == "refresh-2", session.VerificationPending, f.refreshes)
			}
			f.profileFailure = false
			f.quotaFailure = false
			if _, err := (Store{Dir: store.Dir}).quotaWindows(t.Context(), client, "work", credential); err != nil {
				t.Fatal(err)
			}
			if f.refreshes != 2 {
				t.Fatal("pending fresh rotation was redeemed again")
			}
			session, _ = store.readQuota("work")
			if session.VerificationPending {
				t.Fatal("reverified session stayed quarantined")
			}
		})
	}
}

func TestQwenQuotaRejectsWrongOwnerAndRedirect(t *testing.T) {
	store, f, client, _ := quotaFixtureStore(t)
	f.wrongOwner = true
	if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err == nil {
		t.Fatal("another web account was enrolled")
	}
	if _, err := store.readQuota("work"); !errors.Is(err, ErrQuotaLoginRequired) {
		t.Fatal("wrong owner was saved", err)
	}
	f.wrongOwner = false
	f.refreshRedirect = true
	if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err == nil {
		t.Fatal("refresh redirect was accepted")
	}
}

func TestQwenQuotaWaitCannotUndoRemovalOrRelogin(t *testing.T) {
	for _, relogin := range []bool{false, true} {
		t.Run(map[bool]string{false: "remove", true: "relogin"}[relogin], func(t *testing.T) {
			store, _, client, credential := quotaFixtureStore(t)
			if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err != nil {
				t.Fatal(err)
			}
			var ownership sync.Mutex
			ownership.Lock()
			attempted := make(chan struct{})
			store.RefreshTransaction = func(ctx context.Context, read func() error) error {
				close(attempted)
				ownership.Lock()
				defer ownership.Unlock()
				return read()
			}
			done := make(chan error, 1)
			go func() { _, err := store.quotaWindows(t.Context(), client, "work", credential); done <- err }()
			<-attempted
			if relogin {
				credential.RefreshToken = "new-inference-grant"
				_, err := store.SaveManagedCredential("work", credential)
				if err != nil {
					t.Fatal(err)
				}
			} else if err := store.RemoveQuota("work"); err != nil {
				t.Fatal(err)
			}
			ownership.Unlock()
			if err := <-done; !errors.Is(err, ErrQuotaLoginRequired) {
				t.Fatal("stale quota call survived ownership change", err)
			}
			if _, err := store.readQuota("work"); !errors.Is(err, ErrQuotaLoginRequired) {
				t.Fatal("stale quota call recreated session", err)
			}
		})
	}
}

func TestQwenQuotaValidatesResponseUnitsAndCompleteness(t *testing.T) {
	for _, body := range []string{`{"resetAt":1999999999999}`, `{"remainingPercent":50}`, `{"remainingPercent":50,"resetAt":1999999999}`, `{"remainingPercent":101,"resetAt":1999999999999}`, `{"success":false,"httpCode":401,"errorCode":"NOT_LOGIN"}`} {
		t.Run(body, func(t *testing.T) {
			_, f, client, _ := quotaFixtureStore(t)
			f.quotaBody = body
			if _, err := fetchQuota(context.Background(), client, "web-token-0", time.Now()); err == nil {
				t.Fatal("invalid quota accepted")
			}
		})
	}
}

func TestQwenQuotaChangedOwnerQuarantinesRotatedSession(t *testing.T) {
	store, f, client, credential := quotaFixtureStore(t)
	if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err != nil {
		t.Fatal(err)
	}
	session, _ := store.readQuota("work")
	session.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.writeQuota("work", session); err != nil {
		t.Fatal(err)
	}
	f.wrongOwner = true
	before := f.quotaCalls
	if _, err := store.quotaWindows(t.Context(), client, "work", credential); err == nil {
		t.Fatal("changed owner supplied quota")
	}
	session, err := store.readQuota("work")
	if err != nil || !session.VerificationPending || f.quotaCalls != before {
		t.Fatal("unverified owner reached quota or lost rotated session", err)
	}
}

func TestQwenQuotaFailedInitialRefreshCannotBlessUnprovenCookies(t *testing.T) {
	store, f, client, _ := quotaFixtureStore(t)
	f.refreshFailure = true
	if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err == nil {
		t.Fatal("initial refresh failure was hidden")
	}
	session, err := store.readQuotaFile("work", true)
	if err != nil || !session.RenewalPending || !session.VerificationPending {
		t.Fatal("unproven session was not quarantined", err)
	}
	f.refreshFailure = false
	if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 1 {
		t.Fatal("membership accepted cookies without proving refresh ownership")
	}
}

func TestQuotaExpiryUsesAuthoritativeExpiresAt(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	body, _ := json.Marshal(map[string]any{"access_token": "opaque", "expires_at": now.Add(5 * time.Minute).Unix(), "expires_in": 900})
	_, expiry, err := quotaAccessToken(body, now)
	if err != nil || !expiry.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("expiry=%v err=%v", expiry, err)
	}
	for _, value := range []any{true, "bad", now.Add(-time.Second).Unix(), now.Add(48 * time.Hour).Unix()} {
		body, _ = json.Marshal(map[string]any{"access_token": "opaque", "expires_at": value})
		if _, _, err := quotaAccessToken(body, now); err == nil {
			t.Errorf("accepted invalid expires_at %v", value)
		}
	}
}

func TestQuotaPinsWebIdentity(t *testing.T) {
	store, fixture, client, credential := quotaFixtureStore(t)
	fixture.omitWebID = true
	if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err == nil {
		t.Fatal("accepted missing web ID")
	}
	fixture.omitWebID = false
	if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err != nil {
		t.Fatal(err)
	}
	fixture.webID = "different-web-account"
	if _, err := store.quotaWindows(t.Context(), client, "work", credential); err == nil {
		t.Fatal("accepted changed web ID with matching email")
	}
}

func TestQuotaFailedCandidatePreservesActiveAndResumesRotation(t *testing.T) {
	store, fixture, client, _ := quotaFixtureStore(t)
	if err := store.EnrollQuota(t.Context(), client, "work", initialQuotaSession()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(mustQuotaPath(t, store))
	if err != nil {
		t.Fatal(err)
	}
	// A second independent grant is issued. The first sidecar must survive failure.
	candidate := fixture.issueGrant()
	fixture.quotaFailure = true
	if err := store.EnrollQuota(t.Context(), client, "work", candidate); err == nil {
		t.Fatal("candidate unexpectedly succeeded")
	}
	after, err := os.ReadFile(mustQuotaPath(t, store))
	if err != nil || string(before) != string(after) {
		t.Fatal("failed candidate replaced active session", err)
	}
	rotations := fixture.refreshes
	fixture.quotaFailure = false
	if err := store.EnrollQuota(t.Context(), client, "work", candidate); err != nil {
		t.Fatal(err)
	}
	if fixture.refreshes != rotations || fixture.cookieReplays != 0 {
		t.Fatal("retry replayed original candidate cookie")
	}
}
func mustQuotaPath(t *testing.T, store Store) string {
	t.Helper()
	p, err := store.quotaPath("work")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQuotaCookieOnlyEnrollment(t *testing.T) {
	store, fixture, client, _ := quotaFixtureStore(t)
	submitted := initialQuotaSession()
	submitted.AccessToken, submitted.ExpiresAt = "", time.Time{}
	fixture.quotaFailure = true
	if err := store.EnrollQuota(t.Context(), client, "work", submitted); err == nil {
		t.Fatal("accepted failed quota")
	}
	if _, err := store.readQuota("work"); !errors.Is(err, ErrQuotaLoginRequired) {
		t.Fatal("failed candidate became active", err)
	}
	fixture.quotaFailure = false
	if err := store.EnrollQuota(t.Context(), client, "work", submitted); err != nil {
		t.Fatal(err)
	}
	if fixture.refreshes != 1 || fixture.cookieReplays != 0 {
		t.Fatal("replayed cookie-only grant")
	}
	saved, err := store.readQuota("work")
	if err != nil || saved.WebID != "web-account" || saved.VerificationPending {
		t.Fatal("candidate did not become verified", err)
	}
	if err := store.RemoveQuota("work"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.readQuotaFile("work", true); !errors.Is(err, ErrQuotaLoginRequired) {
		t.Fatal("pending credential survived removal", err)
	}
}

type quotaErrorTransport string

func (body quotaErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}
func TestQuotaBusinessAuthenticationError(t *testing.T) {
	for _, code := range []string{"Unauthorized", "forbidden", "Bad_Request"} {
		client := &http.Client{Transport: quotaErrorTransport(`{"success":false,"data":{"code":"` + code + `","details":"secret response"}}`)}
		_, err := webJSON(t.Context(), client, quotaRefreshURL, "")
		if err == nil || strings.Contains(err.Error(), "secret response") {
			t.Fatal("unsafe error", err)
		}
		if errors.Is(err, ErrQuotaLoginRequired) != (code != "Bad_Request") {
			t.Fatalf("wrong classification for %s: %v", code, err)
		}
	}
}
func TestQuotaIgnoresNonRefreshCookies(t *testing.T) {
	jar, err := newQuotaJar(initialQuotaSession().Cookies)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse(quotaRefreshURL)
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "acw_tc", Value: "unneeded", Path: "/"}, {Name: "analytics", Value: "unneeded", Path: "/"}})
	if cookies := jar.snapshot(); len(cookies) != 1 || cookies[0].Name != "refresh_token" {
		t.Fatal("persisted unrelated cookies")
	}
}

func TestQuotaPendingCandidatePinsIdentityBeforeQuotaFailure(t *testing.T) {
	store, fixture, client, _ := quotaFixtureStore(t)
	submitted := initialQuotaSession()
	submitted.AccessToken, submitted.ExpiresAt = "", time.Time{}
	fixture.quotaFailure = true
	if err := store.EnrollQuota(t.Context(), client, "work", submitted); err == nil {
		t.Fatal("expected quota failure")
	}
	pending, err := store.readQuotaFile("work", true)
	if err != nil || pending.WebID != "web-account" {
		t.Fatal("pending rotation lost authoritative identity", err)
	}
	fixture.quotaFailure, fixture.webID = false, "changed-web-account"
	if err := store.EnrollQuota(t.Context(), client, "work", submitted); err == nil {
		t.Fatal("pending retry accepted changed identity")
	}
	if err := store.RemoveQuota("work"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.readQuotaFile("work", true); !errors.Is(err, ErrQuotaLoginRequired) {
		t.Fatal("removal retained candidate", err)
	}
}
func TestQuotaOpaqueTokenRequiresExpiry(t *testing.T) {
	if _, _, err := quotaAccessToken(json.RawMessage(`{"access_token":"opaque"}`), time.Now()); err == nil {
		t.Fatal("guessed missing expiry")
	}
}

func (f *quotaFixture) issueGrant() QuotaSession {
	if f.generation > 0 {
		f.generation++
	}
	session := initialQuotaSession()
	session.AccessToken = "web-token-" + string(rune('0'+f.generation))
	session.Cookies[0].Value = "refresh-" + string(rune('0'+f.generation))
	session.VerificationPending, session.RenewalPending = true, true
	return session
}
func TestQuotaCookieValidationAndDeletion(t *testing.T) {
	valid := initialQuotaSession().Cookies[0]
	for _, mutate := range []func(*QuotaCookie){func(c *QuotaCookie) { c.Value = "" }, func(c *QuotaCookie) { c.Secure = false }, func(c *QuotaCookie) { c.HTTPOnly = false }, func(c *QuotaCookie) { c.Domain = "example.test" }} {
		candidate := valid
		mutate(&candidate)
		if _, err := newQuotaJar([]QuotaCookie{candidate}); err == nil {
			t.Fatal("accepted unsafe imported cookie")
		}
	}
	jar, err := newQuotaJar([]QuotaCookie{valid})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse(quotaRefreshURL)
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "refresh_token", Domain: ".qwen.ai", Path: "/", MaxAge: -1}})
	if len(jar.snapshot()) != 0 || len(jar.Cookies(endpoint)) != 0 {
		t.Fatal("provider deletion did not remove refresh cookie")
	}
}
