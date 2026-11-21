package qwensubscription

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const QuotaSessionVersion = 2
const quotaAuthOrigin = "https://auth.qwen.ai"
const quotaRefreshURL = quotaAuthOrigin + "/api/v2/auths/refresh"

// QuotaSession is an independent, broad web-account sign-in. Its cookies are
// sent only to auth.qwen.ai. Subject and Email are bound by the serving store,
// never accepted as evidence of the caller's inference account identity.
type QuotaSession struct {
	Version             int           `json:"version"`
	AccessToken         string        `json:"access_token"`
	ExpiresAt           time.Time     `json:"expires_at"`
	Cookies             []QuotaCookie `json:"cookies"`
	Subject             string        `json:"subject,omitempty"`
	Email               string        `json:"email,omitempty"`
	VerificationPending bool          `json:"verification_pending,omitempty"`
	RenewalPending      bool          `json:"renewal_pending,omitempty"`
}

type QuotaCookie struct {
	Name     string        `json:"name"`
	Value    string        `json:"value"`
	Domain   string        `json:"domain,omitempty"`
	Path     string        `json:"path"`
	Expires  time.Time     `json:"expires,omitempty"`
	Secure   bool          `json:"secure"`
	HTTPOnly bool          `json:"http_only"`
	SameSite http.SameSite `json:"same_site,omitempty"`
}

// quotaCookieJar retains only this login's auth-host cookies, including their
// expiry and path attributes, so refresh can continue after a process restart.
type quotaCookieJar struct {
	mu      sync.Mutex
	jar     *cookiejar.Jar
	cookies map[string]QuotaCookie
	changed bool
}

func newQuotaJar(saved []QuotaCookie) (*quotaCookieJar, error) {
	if len(saved) > 64 {
		return nil, errors.New("too many Qwen session cookies")
	}
	standard, _ := cookiejar.New(nil)
	jar := &quotaCookieJar{jar: standard, cookies: make(map[string]QuotaCookie)}
	origin, _ := url.Parse(quotaAuthOrigin + "/")
	for _, cookie := range saved {
		if err := validateQuotaCookie(cookie); err != nil {
			return nil, err
		}
		jar.SetCookies(origin, []*http.Cookie{cookie.httpCookie()})
	}
	jar.changed = false
	return jar, nil
}

func validateQuotaCookie(c QuotaCookie) error {
	domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
	if domain != "" && domain != "auth.qwen.ai" && domain != "qwen.ai" {
		return errors.New("Qwen session cookie has an untrusted domain")
	}
	if c.Name != "refresh_token" || c.Value == "" || !c.Secure || !c.HTTPOnly || len(c.Value) > 8192 || !strings.HasPrefix(c.Path, "/") || len(c.Path) > 1024 {
		return errors.New("invalid Qwen session cookie")
	}
	if c.httpCookie().Valid() != nil {
		return errors.New("invalid Qwen session cookie")
	}
	return nil
}

func (c QuotaCookie) httpCookie() *http.Cookie {
	return &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Expires: c.Expires, Secure: c.Secure, HttpOnly: c.HTTPOnly, SameSite: c.SameSite}
}

func quotaCookieHost(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Host == "auth.qwen.ai"
}

func (j *quotaCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if !quotaCookieHost(u) {
		return nil
	}
	return j.jar.Cookies(u)
}

func (j *quotaCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if !quotaCookieHost(u) {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, cookie := range cookies {
		if cookie.Name != "refresh_token" {
			continue
		}
		path := cookie.Path
		if !strings.HasPrefix(path, "/") {
			path = u.Path[:strings.LastIndex(u.Path, "/")+1]
			if path != "/" {
				path = strings.TrimSuffix(path, "/")
			}
		}
		saved := QuotaCookie{Name: cookie.Name, Value: cookie.Value, Domain: strings.ToLower(strings.TrimPrefix(cookie.Domain, ".")), Path: path, Expires: cookie.Expires, Secure: cookie.Secure, HTTPOnly: cookie.HttpOnly, SameSite: cookie.SameSite}
		deleting := cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(time.Now()))
		valid := saved
		if deleting {
			valid.Value, valid.Secure, valid.HTTPOnly = "deleted", true, true
		}
		if validateQuotaCookie(valid) != nil {
			continue
		}
		domain := saved.Domain
		if domain == "" {
			domain = u.Hostname()
		}
		key := saved.Name + "\x00" + domain + "\x00" + saved.Path
		if deleting {
			delete(j.cookies, key)
		} else if len(j.cookies) < 64 || j.cookies[key].Name != "" {
			if cookie.MaxAge > 0 {
				saved.Expires = time.Now().Add(time.Duration(cookie.MaxAge) * time.Second)
			}
			j.cookies[key] = saved
		} else {
			continue
		}
		j.jar.SetCookies(u, []*http.Cookie{cookie})
		j.changed = true
	}
}

func (j *quotaCookieJar) snapshot() []QuotaCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []QuotaCookie
	for _, cookie := range j.cookies {
		if cookie.Expires.IsZero() || cookie.Expires.After(time.Now()) {
			out = append(out, cookie)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name+out[i].Domain+out[i].Path < out[j].Name+out[j].Domain+out[j].Path
	})
	return out
}

func (j *quotaCookieJar) takeChanged() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	changed := j.changed
	j.changed = false
	return changed
}

func quotaHTTPClient(base *http.Client, jar *quotaCookieJar) *http.Client {
	client := &http.Client{Timeout: 15 * time.Second}
	if base != nil {
		*client = *base
	}
	if client.Timeout == 0 {
		client.Timeout = 15 * time.Second
	}
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func quotaAccessToken(body json.RawMessage, now time.Time) (string, time.Time, error) {
	var response struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		ExpiresAt   *int64 `json:"expires_at"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "", time.Time{}, errors.New("invalid Qwen session response")
	}
	token, err := normalizeQuotaToken(response.AccessToken)
	if err != nil {
		return "", time.Time{}, err
	}
	var expiry time.Time
	if response.ExpiresAt == nil && response.ExpiresIn != 0 {
		if response.ExpiresIn < 0 || response.ExpiresIn > 24*60*60 {
			return "", time.Time{}, errors.New("invalid Qwen access-token expiry")
		}
		expiry = now.Add(time.Duration(response.ExpiresIn) * time.Second)
	}
	if parts := strings.Split(token, "."); response.ExpiresAt == nil && expiry.IsZero() && len(parts) == 3 {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Exp int64 `json:"exp"`
		}
		if decodeErr == nil && json.Unmarshal(decoded, &claims) == nil && claims.Exp > 0 {
			expiry = time.Unix(claims.Exp, 0).UTC()
		}
	}
	if response.ExpiresAt != nil {
		expiry = time.Unix(*response.ExpiresAt, 0).UTC()
	}
	if expiry.IsZero() {
		return "", time.Time{}, errors.New("Qwen session response omitted access-token expiry")
	}
	if expiry.After(now.Add(24 * time.Hour)) {
		return "", time.Time{}, errors.New("invalid Qwen access-token expiry")
	}
	if !expiry.After(now) {
		return "", time.Time{}, ErrQuotaLoginRequired
	}
	return token, expiry, nil
}
