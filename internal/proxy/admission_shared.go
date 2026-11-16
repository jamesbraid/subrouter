package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

const sharedAdmissionEnv = "SUBROUTER_ADMISSION_DIR"

// File locks keep a physical-attempt limit shared across worker generations.
// A crashed worker releases its slots through descriptor closure, without a
// lease timeout that could admit a second request while the first still runs.
type sharedAdmissionStore struct {
	dir    string
	once   sync.Once
	err    error
	active *os.File
}

type sharedAdmissionSlot struct{ file *os.File }

type sharedAdmissionWaiter struct {
	path   string
	file   *os.File
	closed atomic.Bool
}

func sharedAdmissionDigest(key admissionKey) string {
	digest := sha256.Sum256([]byte(string(key.provider) + "\x00" + key.account + "\x00" + key.model))
	return fmt.Sprintf("%x", digest)
}

func (w *sharedAdmissionWaiter) closeLocked() {
	if w != nil && w.closed.CompareAndSwap(false, true) {
		_ = w.file.Close()
		_ = os.Remove(w.path)
	}
}

func (w *sharedAdmissionWaiter) release() {
	if w != nil && w.closed.CompareAndSwap(false, true) {
		// Cancellation must not wait for another generation's metadata lock.
		// The next queue scan removes this now-unlocked entry.
		_ = w.file.Close()
	}
}

func (s *sharedAdmissionSlot) release() {
	if s != nil {
		_ = s.file.Close()
	}
}

func (s *sharedAdmissionStore) prepare(limit int, overrides map[admissionLimitKey]int) error {
	if s == nil {
		return nil
	}
	s.once.Do(func() { s.err = s.openPolicy(limit, overrides) })
	return s.err
}

func (s *sharedAdmissionStore) openPolicy(limit int, overrides map[admissionLimitKey]int) error {
	if !filepath.IsAbs(s.dir) {
		return fmt.Errorf("%s must be an absolute directory", sharedAdmissionEnv)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create shared admission directory: %w", err)
	}
	policy := struct {
		Version int              `json:"version"`
		Default int              `json:"default_limit"`
		Limits  []AdmissionLimit `json:"limits"`
	}{Version: 1, Default: limit}
	for key, value := range overrides {
		policy.Limits = append(policy.Limits, AdmissionLimit{Provider: key.provider, Model: key.model, Limit: value})
	}
	sort.Slice(policy.Limits, func(i, j int) bool {
		a, b := policy.Limits[i], policy.Limits[j]
		return a.Provider < b.Provider || a.Provider == b.Provider && a.Model < b.Model
	})
	wanted, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	guard, err := os.OpenFile(filepath.Join(s.dir, "policy.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer guard.Close()
	if _, err := lockAdmissionFile(guard, true, false); err != nil {
		return err
	}
	activePath := filepath.Join(s.dir, "active.lock")
	active, err := os.OpenFile(activePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = active.Close()
		}
	}()
	current, err := os.ReadFile(filepath.Join(s.dir, "policy.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !bytes.Equal(current, wanted) {
		free, err := lockAdmissionFile(active, true, true)
		if err != nil {
			return err
		}
		if !free {
			return fmt.Errorf("admission limits differ from a running worker generation")
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(s.dir, "policy.json"), wanted, 0o600); err != nil {
			return err
		}
		// The policy guard serializes startup, so no worker can change policy
		// between dropping the exclusive lifetime lock and taking a shared one.
		_ = active.Close()
		active, err = os.OpenFile(activePath, os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
	}
	if _, err := lockAdmissionFile(active, false, false); err != nil {
		return err
	}
	s.active, installed = active, true
	return nil
}

func (s *sharedAdmissionStore) queueGuard(digest string) (*os.File, bool, error) {
	file, err := os.OpenFile(filepath.Join(s.dir, digest+".queue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	available, err := lockAdmissionFile(file, true, true)
	if err != nil || !available {
		_ = file.Close()
		return nil, false, err
	}
	return file, true, nil
}

func (s *sharedAdmissionStore) queueHeadLocked(digest string) (string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", err
	}
	prefix := digest + "-wait-"
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		sequence := strings.TrimPrefix(entry.Name(), prefix)
		if len(sequence) != 20 {
			continue
		}
		if _, err := strconv.ParseUint(sequence, 10, 64); err != nil {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		file, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		free, err := lockAdmissionFile(file, true, true)
		_ = file.Close()
		if free {
			// The owner cancelled or exited. Its descriptor, not a PID or
			// timeout, determines whether the queue entry is still live.
			_ = os.Remove(path)
		}
		if err != nil {
			return "", err
		}
		if !free {
			return path, nil
		}
	}
	return "", nil
}

func (s *sharedAdmissionStore) enqueueLocked(digest string) (*sharedAdmissionWaiter, error) {
	sequencePath := filepath.Join(s.dir, digest+".sequence")
	var sequence uint64
	if data, err := os.ReadFile(sequencePath); err == nil {
		sequence, err = strconv.ParseUint(string(data), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid shared admission queue sequence: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if sequence == ^uint64(0) {
		return nil, fmt.Errorf("shared admission queue sequence exhausted")
	}
	sequence++
	if err := fsutil.ReplaceFile(sequencePath, []byte(strconv.FormatUint(sequence, 10)), 0o600); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, fmt.Sprintf("%s-wait-%020d", digest, sequence))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := lockAdmissionFile(file, true, true)
	if err != nil || !locked {
		_ = file.Close()
		_ = os.Remove(path)
		if err != nil {
			return nil, fmt.Errorf("lock shared admission waiter: %w", err)
		}
		return nil, fmt.Errorf("new shared admission waiter is already locked")
	}
	return &sharedAdmissionWaiter{path: path, file: file}, nil
}

func (s *sharedAdmissionStore) tryAcquire(key admissionKey, limit int, pending **sharedAdmissionWaiter, enqueue bool) (*sharedAdmissionSlot, bool, error) {
	if s == nil {
		return nil, true, nil
	}
	name := sharedAdmissionDigest(key)
	guard, available, err := s.queueGuard(name)
	if err != nil {
		return nil, false, err
	}
	if !available {
		return nil, false, nil
	}
	defer guard.Close()
	var waiter *sharedAdmissionWaiter
	if pending != nil {
		waiter = *pending
	}
	head, err := s.queueHeadLocked(name)
	if err != nil {
		return nil, false, err
	}
	canStart := head == "" || waiter != nil && head == waiter.path
	if canStart {
		for slot := 0; slot < limit; slot++ {
			path := filepath.Join(s.dir, fmt.Sprintf("%s-%02d.lock", name, slot))
			file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				return nil, false, err
			}
			locked, err := lockAdmissionFile(file, true, true)
			if err != nil || !locked {
				_ = file.Close()
				if err != nil {
					return nil, false, err
				}
				continue
			}
			waiter.closeLocked()
			if pending != nil {
				*pending = nil
			}
			return &sharedAdmissionSlot{file: file}, true, nil
		}
	}
	if enqueue && pending != nil && *pending == nil {
		*pending, err = s.enqueueLocked(name)
		if err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}

func (s *sharedAdmissionStore) capacity(key admissionKey, limit int, ownHead *sharedAdmissionWaiter) (bool, int, error) {
	name := sharedAdmissionDigest(key)
	guard, available, err := s.queueGuard(name)
	if err != nil || !available {
		return false, limit, err
	}
	defer guard.Close()
	head, err := s.queueHeadLocked(name)
	if err != nil {
		return false, limit, err
	}
	active, err := s.activeCount(key, limit)
	if err != nil {
		return false, limit, err
	}
	load := active
	if head != "" && (ownHead == nil || head != ownHead.path) {
		load++
	}
	return head == "" && active < limit, load, nil
}

func (s *sharedAdmissionStore) activeCount(key admissionKey, limit int) (int, error) {
	digest := sharedAdmissionDigest(key)
	active := 0
	for slot := 0; slot < limit; slot++ {
		path := filepath.Join(s.dir, fmt.Sprintf("%s-%02d.lock", digest, slot))
		file, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		free, err := lockAdmissionFile(file, true, true)
		_ = file.Close()
		if err != nil {
			return 0, err
		}
		if !free {
			active++
		}
	}
	return active, nil
}
