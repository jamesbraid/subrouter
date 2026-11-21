package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/qwensubscription"
	"github.com/manaflow-ai/subrouter/internal/fsutil"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// Qwen's browser challenge owns sign-in. Capture only the new profile's
// persistent refresh cookie after Firefox exits, then give refresh ownership
// to the selected router. An automation session changes Firefox's login state.
func authorizeQwenQuotaBrowser(ctx context.Context, out io.Writer, email string) (qwensubscription.QuotaSession, error) {
	firefox := "firefox"
	if runtime.GOOS == "darwin" {
		firefox = "/Applications/Firefox.app/Contents/MacOS/firefox"
	}
	firefox, err := exec.LookPath(firefox)
	if err != nil {
		return qwensubscription.QuotaSession{}, errors.New("Qwen quota sign-in requires Firefox")
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return qwensubscription.QuotaSession{}, errors.New("Qwen quota sign-in requires sqlite3 with JSON output support")
	}
	if _, err := qwenCookieQuery(ctx, sqlite, ":memory:", "SELECT 1 AS probe"); err != nil {
		return qwensubscription.QuotaSession{}, errors.New("Qwen quota sign-in requires sqlite3 with read-only JSON output support")
	}
	root := filepath.Join(storepath.StateDir(), "tmp")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return qwensubscription.QuotaSession{}, err
	}
	profile, err := os.MkdirTemp(root, "qwen-quota-")
	if err != nil {
		return qwensubscription.QuotaSession{}, err
	}
	defer os.RemoveAll(profile)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	fmt.Fprintf(out, "Sign in as %s on Qwen's official page, then Quit this dedicated Firefox instance.\n", email)
	fmt.Fprintln(out, "Only its Qwen refresh cookie will be read after exit. Your existing Firefox profiles are not accessed.")
	command := exec.CommandContext(ctx, firefox, "-foreground", "--no-remote", "--new-instance", "--profile", profile, "https://chat.qwen.ai/auth")
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return qwensubscription.QuotaSession{}, ctx.Err()
		}
		return qwensubscription.QuotaSession{}, errors.New("the dedicated Qwen sign-in browser did not exit successfully")
	}
	database := filepath.Join(profile, "cookies.sqlite")
	info, err := os.Lstat(database)
	if err != nil || !info.Mode().IsRegular() {
		return qwensubscription.QuotaSession{}, errors.New("Qwen sign-in did not save a cookie database in its dedicated profile")
	}
	const query = `SELECT name,value,host,path,expiry,isSecure,isHttpOnly FROM moz_cookies WHERE name='refresh_token' AND host IN ('auth.qwen.ai','.auth.qwen.ai','qwen.ai','.qwen.ai') AND originAttributes='' LIMIT 8`
	body, err := qwenCookieQuery(ctx, sqlite, database, query)
	if err != nil {
		return qwensubscription.QuotaSession{}, errors.New("could not read the dedicated Qwen sign-in cookie")
	}
	cookie, err := decodeQwenRefreshCookie(body, time.Now())
	if err != nil {
		return qwensubscription.QuotaSession{}, err
	}
	return qwensubscription.QuotaSession{Version: qwensubscription.QuotaSessionVersion, Cookies: []qwensubscription.QuotaCookie{cookie}}, nil
}

type qwenCookieOutput struct{ bytes.Buffer }

func (b *qwenCookieOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 64<<10 {
		return 0, errors.New("Qwen cookie query exceeded its output limit")
	}
	return b.Buffer.Write(p)
}

func qwenCookieQuery(ctx context.Context, sqlite, database, query string) ([]byte, error) {
	command := exec.CommandContext(ctx, sqlite, "-readonly", "-json", database, query)
	var output qwenCookieOutput
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func decodeQwenRefreshCookie(body []byte, now time.Time) (qwensubscription.QuotaCookie, error) {
	var rows []struct {
		Name, Value, Host, Path string
		Expiry                  int64
		Secure                  int `json:"isSecure"`
		HTTPOnly                int `json:"isHttpOnly"`
	}
	if json.Unmarshal(body, &rows) != nil {
		return qwensubscription.QuotaCookie{}, errors.New("invalid dedicated Qwen cookie query result")
	}
	var found []qwensubscription.QuotaCookie
	for _, row := range rows {
		switch row.Host {
		case "auth.qwen.ai", ".auth.qwen.ai", ".qwen.ai":
		default:
			return qwensubscription.QuotaCookie{}, errors.New("dedicated Qwen cookie has an unexpected domain")
		}
		if row.Expiry > 10_000_000_000 {
			row.Expiry /= 1000 // Current Firefox persists milliseconds; older releases used seconds.
		}
		if row.Name != "refresh_token" || row.Value == "" || row.Secure != 1 || row.HTTPOnly != 1 || row.Path != "/" || !time.Unix(row.Expiry, 0).After(now) {
			continue
		}
		domain := row.Host
		if !strings.HasPrefix(domain, ".") {
			domain = "" // Preserve a host-only cookie when rebuilding the auth-origin jar.
		}
		found = append(found, qwensubscription.QuotaCookie{Name: row.Name, Value: row.Value, Domain: domain, Path: row.Path, Expires: time.Unix(row.Expiry, 0), Secure: true, HTTPOnly: true})
	}
	if len(found) != 1 {
		return qwensubscription.QuotaCookie{}, errors.New("Qwen sign-in did not save one unambiguous persistent refresh cookie; complete sign-in before quitting the dedicated browser")
	}
	return found[0], nil
}

// Keep the original grant until import acknowledges ownership. Retrying an
// ambiguous response must reuse this enrollment rather than opening a browser.
type qwenQuotaPending struct {
	Email   string                        `json:"email"`
	Session qwensubscription.QuotaSession `json:"session"`
}

func (r srRunner) qwenQuotaPendingPath(server *srServerConfig, label string) string {
	scope := "local:" + r.store.StoreDir()
	if server != nil {
		scope = server.URL + "\x00" + server.TenantKey
	}
	id, _ := qwensubscription.ManagedAccountID(label)
	hash := sha256.Sum256([]byte(scope + "\x00" + id))
	return filepath.Join(storepath.StateDir(), "qwen-quota-pending", fmt.Sprintf("%x.json", hash[:]))
}

func readQwenQuotaPending(path, email string) (qwensubscription.QuotaSession, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return qwensubscription.QuotaSession{}, false, nil
	}
	if err != nil {
		return qwensubscription.QuotaSession{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 64<<10 {
		return qwensubscription.QuotaSession{}, false, errors.New("pending Qwen quota credential must be a private regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return qwensubscription.QuotaSession{}, false, err
	}
	var pending qwenQuotaPending
	if json.Unmarshal(body, &pending) != nil || !strings.EqualFold(pending.Email, email) || pending.Session.Version != qwensubscription.QuotaSessionVersion || pending.Session.AccessToken != "" || len(pending.Session.Cookies) != 1 {
		return qwensubscription.QuotaSession{}, false, errors.New("pending Qwen quota enrollment does not match the selected account")
	}
	return pending.Session, true, nil
}

func saveQwenQuotaPending(path, email string, session qwensubscription.QuotaSession) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(qwenQuotaPending{Email: email, Session: session})
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, body, 0o600)
}

func removeQwenQuotaPending(path string) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return fsutil.SyncDir(filepath.Dir(path))
}
