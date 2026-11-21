package qwensubscription

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

const webProfileURL = "https://auth.qwen.ai/api/v2/auths/"
const quotaURL = "https://sg-entitlement.qwen.com/api/entitlement/agent/usage"

var ErrQuotaLoginRequired = errors.New("Qwen quota login required; run sr qwen subscription quota-login <label>")

func (s Store) quotaPath(label string) (string, error) {
	path, err := s.path(label)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir(), "quota", filepath.Base(path)), nil
}

func normalizeQuotaToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if len(token) > 7 && strings.EqualFold(token[:7], "Bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	if token == "" || len(token) > 16384 {
		return "", errors.New("invalid Qwen quota authorization")
	}
	for _, r := range token {
		if r < 33 || r > 126 {
			return "", errors.New("invalid Qwen quota authorization")
		}
	}
	return token, nil
}

// webJSON deliberately treats HTTP-200 business authentication errors as
// failures and never includes a response body, URL query, or token in errors.
func webJSON(ctx context.Context, client *http.Client, endpoint, token string) (json.RawMessage, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" ||
		(parsed.Host != "auth.qwen.ai" && parsed.Host != "sg-entitlement.qwen.com") {
		return nil, errors.New("invalid Qwen quota endpoint")
	}
	if parsed.Host == "sg-entitlement.qwen.com" {
		if parsed.Path != "/api/entitlement/agent/usage" {
			return nil, errors.New("invalid Qwen entitlement endpoint")
		}
		query := parsed.Query()
		query.Set("biz_id", "ai_qwen")
		query.Set("chat_client", "h5")
		query.Set("device", "pc")
		query.Set("pr", "qwen-i18n")
		parsed.RawQuery = query.Encode()
	} else {
		switch endpoint {
		case quotaRefreshURL, webProfileURL:
		default:
			return nil, errors.New("invalid Qwen auth endpoint")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, errors.New("could not prepare Qwen quota request")
	}
	if token != "" {
		if endpoint != webProfileURL && parsed.Host != "sg-entitlement.qwen.com" {
			return nil, errors.New("Qwen bearer is not allowed on this endpoint")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", "subrouter")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://chat.qwen.ai")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Version", "0.3.49")
	req.Header.Set("source", "h5")
	req.Header.Set("Timezone", time.Now().Format("Mon Jan 02 2006 15:04:05 GMT-0700"))
	req.Header.Set("Referer", "https://chat.qwen.ai/")
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, errors.New("could not prepare Qwen request identity")
	}
	id[6], id[8] = (id[6]&0x0f)|0x40, (id[8]&0x3f)|0x80
	req.Header.Set("X-Request-Id", fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]))
	if parsed.Host == "auth.qwen.ai" {
		req.Header.Set("x-request-origin", "https://chat.qwen.ai")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	checked := *client
	if checked.Timeout == 0 {
		checked.Timeout = 15 * time.Second
	}
	checked.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := checked.Do(req)
	if err != nil {
		return nil, errors.New("Qwen quota request failed")
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return nil, ErrQuotaLoginRequired
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Qwen quota returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("invalid Qwen quota response")
	}
	var envelope struct {
		Success   *bool           `json:"success"`
		HTTPCode  int             `json:"httpCode"`
		ErrorCode string          `json:"errorCode"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {

		return nil, errors.New("invalid Qwen quota response")
	}
	if envelope.HTTPCode == 401 || envelope.HTTPCode == 403 || envelope.ErrorCode == "NOT_LOGIN" {
		return nil, ErrQuotaLoginRequired
	}
	if envelope.Success != nil && !*envelope.Success {
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(envelope.Data, &failure)
		switch strings.ToLower(failure.Code) {
		case "unauthorized", "invalid token", "forbidden":
			return nil, ErrQuotaLoginRequired
		}

		return nil, errors.New("Qwen quota request was rejected")
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		return envelope.Data, nil
	}
	return body, nil
}

func verifyQuotaOwner(ctx context.Context, client *http.Client, token, expectedEmail string) (string, error) {
	if strings.TrimSpace(expectedEmail) == "" {
		return "", errors.New("enrolled Qwen account has no verified email")
	}
	body, err := webJSON(ctx, client, webProfileURL, token)
	if err != nil {
		return "", err
	}
	var profile struct {
		Email string `json:"email"`
		ID    string `json:"id"`
	}
	if json.Unmarshal(body, &profile) != nil || strings.TrimSpace(profile.Email) == "" {
		return "", errors.New("Qwen quota sign-in has no verified email")
	}
	if !strings.EqualFold(strings.TrimSpace(profile.Email), strings.TrimSpace(expectedEmail)) {
		return "", errors.New("Qwen quota sign-in belongs to another account")
	}
	if strings.TrimSpace(profile.ID) == "" {
		return "", errors.New("Qwen quota sign-in has no stable account identity")
	}
	return profile.ID, nil
}

func fetchQuota(ctx context.Context, client *http.Client, token string, now time.Time) ([]accounts.UsageWindow, error) {
	body, err := webJSON(ctx, client, quotaURL, token)
	if err != nil {
		return nil, err
	}
	var usage struct {
		RemainingPercent *float64 `json:"remainingPercent"`
		ResetAt          *int64   `json:"resetAt"`
	}
	if json.Unmarshal(body, &usage) != nil || usage.RemainingPercent == nil || usage.ResetAt == nil {
		return nil, errors.New("Qwen quota response omitted remaining percentage or reset")
	}
	remaining := *usage.RemainingPercent
	if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 100 {
		return nil, errors.New("Qwen quota response has an invalid percentage")
	}
	// Qwen's web app feeds resetAt directly to Date: it is epoch milliseconds.
	reset := time.UnixMilli(*usage.ResetAt).UTC()
	if !reset.After(now) || reset.After(now.Add(8*24*time.Hour)) {
		return nil, errors.New("Qwen weekly quota reset is stale or invalid")
	}
	return []accounts.UsageWindow{{Name: "weekly", UsedPercent: 100 - remaining, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: reset, ResetAfterSeconds: int64(reset.Sub(now).Seconds())}}, nil
}

// The submitted grant and the server's renewal progress have different owners.
// Keep the original-grant fingerprint private so import callers cannot supply it.
type storedQuotaSession struct {
	QuotaSession
	EnrollmentFingerprint string `json:"enrollment_fingerprint"`
	WebID                 string `json:"web_id"`
	pending               bool
}

// Cookie authority identifies the renewable grant. Expiry, access-token rotation,
// and client-supplied progress flags must not turn a retry into a new enrollment.
func quotaEnrollmentFingerprint(cookies []QuotaCookie) (string, error) {
	if len(cookies) == 0 || len(cookies) > 64 {
		return "", errors.New("invalid Qwen enrollment cookies")
	}
	authority := make(map[string]string, len(cookies))
	for _, cookie := range cookies {
		if err := validateQuotaCookie(cookie); err != nil {
			return "", err
		}
		domain := strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
		if domain == "" {
			domain = "auth.qwen.ai"
		}
		authority[cookie.Name+"\x00"+domain+"\x00"+cookie.Path] = cookie.Value
	}
	body, _ := json.Marshal(authority)
	fingerprint := sha256.Sum256(body)
	return hex.EncodeToString(fingerprint[:]), nil
}

// EnrollQuota takes refresh ownership of a new isolated web session. The caller
// owns the account transaction; user interaction must have finished beforehand.
func (s Store) EnrollQuota(ctx context.Context, client *http.Client, label string, submitted QuotaSession) error {
	credential, ok, err := s.read(label)
	if err != nil {
		return err
	}
	if !ok || credential.VerificationPending {
		return errors.New("verified Qwen subscription account was not found")
	}
	if submitted.Version != QuotaSessionVersion {
		return errors.New("Qwen quota enrollment requires a version 2 renewable session")
	}
	fingerprint, err := quotaEnrollmentFingerprint(submitted.Cookies)
	if err != nil {
		return err
	}
	for _, pending := range []bool{false, true} {
		current, readErr := s.readQuotaFile(label, pending)
		if readErr != nil && !errors.Is(readErr, ErrQuotaLoginRequired) {
			return readErr
		}
		if readErr != nil || current.EnrollmentFingerprint != fingerprint {
			continue
		}
		if current.Subject != credential.Subject || !strings.EqualFold(current.Email, credential.Email) {
			return errors.New("Qwen quota enrollment belongs to a replaced inference account")
		}
		if !pending && !current.VerificationPending && !current.RenewalPending && current.ExpiresAt.After(time.Now().Add(30*time.Second)) {
			return nil
		}
		if _, err := s.pollQuotaSession(ctx, client, label, &current); err != nil {
			return err
		}
		if pending {
			return s.promoteQuota(label, current)
		}
		return nil
	}

	session := storedQuotaSession{QuotaSession: submitted, EnrollmentFingerprint: fingerprint, pending: true}
	jar, err := newQuotaJar(session.Cookies)
	if err != nil {
		return err
	}
	if session.AccessToken != "" {
		tokenBody, _ := json.Marshal(map[string]any{"access_token": session.AccessToken, "expires_at": session.ExpiresAt.Unix()})
		session.AccessToken, session.ExpiresAt, err = quotaAccessToken(tokenBody, time.Now())
		if err != nil {
			return err
		}
		checked := quotaHTTPClient(client, jar)
		session.WebID, err = verifyQuotaOwner(ctx, checked, session.AccessToken, credential.Email)
		if err != nil {
			return err
		}
	}
	session.Subject, session.Email = credential.Subject, credential.Email
	session.Cookies, session.VerificationPending = jar.snapshot(), true
	session.RenewalPending = true
	if err := s.writeQuota(label, session); err != nil {
		return err
	}
	if _, err = s.pollQuotaSession(ctx, client, label, &session); err != nil {
		return err
	}
	return s.promoteQuota(label, session)
}

func (s Store) QuotaAccountEmail(label string) (string, error) {
	credential, exists, err := s.read(label)
	if err != nil {
		return "", err
	}
	if !exists || credential.VerificationPending || credential.Email == "" {
		return "", errors.New("verified Qwen subscription email was not found")
	}
	return credential.Email, nil
}

// A failed enrollment keeps its rotated grant private without replacing live telemetry.
func (s Store) promoteQuota(label string, session storedQuotaSession) error {
	session.pending = false
	if err := s.writeQuota(label, session); err != nil {
		return err
	}
	path, err := s.quotaPath(label)
	if err != nil {
		return err
	}
	if err := os.Remove(path + ".pending"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(path))
}

func (s Store) writeQuota(label string, session storedQuotaSession) error {
	path, err := s.quotaPath(label)
	if err != nil {
		return err
	}
	if session.pending {
		path += ".pending"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(session)
	if err != nil {
		return err
	}
	if len(body) > 128<<10 {
		return errors.New("Qwen quota session is too large")
	}
	if err := fsutil.ReplaceFile(path, body, 0o600); err != nil {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(path))
}

func (s Store) readQuota(label string) (storedQuotaSession, error) {
	return s.readQuotaFile(label, false)
}

func (s Store) readQuotaFile(label string, pending bool) (storedQuotaSession, error) {
	path, err := s.quotaPath(label)
	if err != nil {
		return storedQuotaSession{}, err
	}
	if pending {
		path += ".pending"
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return storedQuotaSession{}, ErrQuotaLoginRequired
	}
	if err != nil {
		return storedQuotaSession{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 128<<10 {
		return storedQuotaSession{}, errors.New("invalid Qwen quota session file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return storedQuotaSession{}, err
	}
	var session storedQuotaSession
	if json.Unmarshal(body, &session) != nil || session.Version != QuotaSessionVersion || len(session.Cookies) == 0 {
		return session, ErrQuotaLoginRequired
	}
	if session.AccessToken == "" {
		if !pending || !session.RenewalPending {
			return session, ErrQuotaLoginRequired
		}
	} else {
		if _, err := normalizeQuotaToken(session.AccessToken); err != nil || session.ExpiresAt.IsZero() {
			return session, ErrQuotaLoginRequired
		}
	}
	if _, err := newQuotaJar(session.Cookies); err != nil {
		return session, ErrQuotaLoginRequired
	}
	session.pending = pending
	return session, nil
}

func (s Store) RemoveQuota(label string) error {
	path, err := s.quotaPath(label)
	if err != nil {
		return err
	}
	removed := false
	for _, name := range []string{path, path + ".pending"} {
		if err := os.Remove(name); err == nil {
			removed = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if removed {
		return fsutil.SyncDir(filepath.Dir(path))
	}
	return nil
}

func (s Store) renewQuota(ctx context.Context, client *http.Client, jar *quotaCookieJar, label string, session *storedQuotaSession) error {
	session.RenewalPending = true
	body, requestErr := webJSON(ctx, client, quotaRefreshURL, "")
	// Preserve cookie rotation even if the response cannot supply a usable token.
	session.Cookies = jar.snapshot()
	if requestErr == nil {
		token, expiry, tokenErr := quotaAccessToken(body, time.Now())
		if tokenErr == nil {
			session.AccessToken, session.ExpiresAt = token, expiry
			session.RenewalPending = false
			refreshEndpoint, _ := url.Parse(quotaRefreshURL)
			if len(jar.Cookies(refreshEndpoint)) == 0 {
				session.RenewalPending = true
				requestErr = ErrQuotaLoginRequired
			}
		}
		if tokenErr != nil {
			requestErr = tokenErr
		}
	}
	session.VerificationPending = true
	// No network operation or cancellation check may precede this durable write.
	if err := s.writeQuota(label, *session); err != nil {
		return err
	}
	jar.takeChanged()
	return requestErr
}

func (s Store) verifyQuota(ctx context.Context, client *http.Client, jar *quotaCookieJar, label string, session *storedQuotaSession) ([]accounts.UsageWindow, error) {
	identityChanged := false
	webID, err := verifyQuotaOwner(ctx, client, session.AccessToken, session.Email)
	if err == nil && session.WebID != "" && session.WebID != webID {
		err = errors.New("Qwen quota account identity changed")
	}
	if err == nil && session.WebID == "" {
		session.WebID, session.VerificationPending = webID, true
		identityChanged = true
	}
	if jar.takeChanged() || identityChanged {
		session.Cookies, session.VerificationPending = jar.snapshot(), true
		if writeErr := s.writeQuota(label, *session); writeErr != nil {
			return nil, writeErr
		}
	}
	if err != nil {
		return nil, err
	}
	windows, err := fetchQuota(ctx, client, session.AccessToken, time.Now())
	if err != nil {
		return nil, err
	}
	if session.VerificationPending {
		session.VerificationPending = false
		if err := s.writeQuota(label, *session); err != nil {
			return nil, err
		}
	}
	return windows, nil
}

// A supplied transaction is the sole serving ownership lock. Taking a separate
// quota lock before it would invert inference-refresh and account-removal order.
var standaloneQuotaGate = make(chan struct{}, 1)

func (s Store) quotaWindows(ctx context.Context, client *http.Client, label string, expected CredentialInfo) (windows []accounts.UsageWindow, err error) {
	// Optional telemetry without an enrolled session is a read-only miss. In
	// particular it must not publish a standalone CLI account mutation.
	if _, err := s.readQuota(label); err != nil {
		return nil, err
	}
	read := func() error {
		credential, exists, readErr := s.read(label)
		if readErr != nil {
			return readErr
		}
		if !exists || credential.VerificationPending || credential.Subject != expected.Subject || !strings.EqualFold(credential.Email, expected.Email) {
			return ErrQuotaLoginRequired
		}
		session, readErr := s.readQuota(label)
		if readErr != nil {
			return readErr
		}
		if session.Subject != credential.Subject || !strings.EqualFold(session.Email, credential.Email) {
			return ErrQuotaLoginRequired
		}
		windows, readErr = s.pollQuotaSession(ctx, client, label, &session)
		return readErr
	}
	if s.RefreshTransaction != nil {
		err = s.RefreshTransaction(ctx, read)
	} else {
		select {
		case standaloneQuotaGate <- struct{}{}:
			defer func() { <-standaloneQuotaGate }()
			err = read()
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	return windows, err
}

// The caller already owns the account transaction. Reusing this path for import
// retries avoids nesting refresh ownership or redeeming the submitted cookie.
func (s Store) pollQuotaSession(ctx context.Context, client *http.Client, label string, session *storedQuotaSession) ([]accounts.UsageWindow, error) {
	jar, err := newQuotaJar(session.Cookies)
	if err != nil {
		return nil, err
	}
	checked := quotaHTTPClient(client, jar)
	renewed := false
	if session.RenewalPending || !session.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		if err := s.renewQuota(ctx, checked, jar, label, session); err != nil {
			return nil, err
		}
		renewed = true
	}
	windows, err := s.verifyQuota(ctx, checked, jar, label, session)
	if errors.Is(err, ErrQuotaLoginRequired) && !renewed {
		if err := s.renewQuota(ctx, checked, jar, label, session); err != nil {
			return nil, err
		}
		windows, err = s.verifyQuota(ctx, checked, jar, label, session)
	}
	return windows, err
}
