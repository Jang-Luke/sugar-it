// Package config persists the daemon's settings and the control token.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"sugarintegration/internal/mesh"
	"sugarintegration/internal/shares"
)

const (
	configFile = "config.json"
	tokenFile  = "control.token"
	// fallbackName is used when the hostname has no usable characters.
	fallbackName = "host"
)

type Config struct {
	Name              string        `json:"name"`
	ListenAddr        string        `json:"listen_addr"`
	ControlAddr       string        `json:"control_addr"`
	DAVAddr           string        `json:"dav_addr"`
	AdvertiseAddrs    []string      `json:"advertise_addrs,omitempty"`
	MaxConcurrentJobs int           `json:"max_concurrent_jobs"`
	Shares            []shares.Spec `json:"shares"`
	Members           []mesh.Member `json:"members"`
}

// DefaultDir is the config directory used when --config-dir is not given.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sugar-it"), nil
}

func defaults() Config {
	h, _ := os.Hostname()
	return Config{
		Name:              defaultName(h),
		ListenAddr:        ":7443",
		ControlAddr:       "127.0.0.1:7444",
		DAVAddr:           "127.0.0.1:7445",
		MaxConcurrentJobs: 2,
	}
}

// defaultName normalises a hostname to [a-z0-9-]{1,32}; a name that ends up
// empty or reserved ("local") becomes "host".
func defaultName(hostname string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(hostname) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	n := strings.Trim(b.String(), "-")
	if len(n) > 32 {
		n = strings.TrimRight(n[:32], "-")
	}
	if !mesh.ValidName(n) {
		return fallbackName
	}
	return n
}

func (c Config) clone() Config {
	c.AdvertiseAddrs = slices.Clone(c.AdvertiseAddrs)
	c.Shares = slices.Clone(c.Shares)
	c.Members = slices.Clone(c.Members)
	for i := range c.Members {
		c.Members[i].Addrs = slices.Clone(c.Members[i].Addrs)
	}
	return c
}

type Store struct {
	mu  sync.Mutex
	dir string
	cur Config
}

// Open loads dir/config.json, writing the defaults if it does not exist. A
// file that cannot be parsed is an error and is left untouched.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	path := filepath.Join(dir, configFile)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.cur = defaults()
		if err := writeAtomic(path, mustJSON(s.cur), 0o600); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		s.cur = defaults()
		if err := json.Unmarshal(data, &s.cur); err != nil {
			return nil, fmt.Errorf("config: %s is not valid JSON: %w", path, err)
		}
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

// Get returns a deep copy of the current configuration.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.clone()
}

// Update applies fn to a copy, saves it, and only then makes it current. If
// fn or the save fails, neither memory nor the file changes.
func (s *Store) Update(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur.clone()
	if err := fn(&next); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.dir, configFile), mustJSON(next), 0o600); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// ControlToken returns the control API token, creating it (mode 0600) if absent.
func ControlToken(dir string) (string, error) {
	path := filepath.Join(dir, tokenFile)
	for range 3 {
		if b, err := os.ReadFile(path); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				return t, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
		tok := base64.RawURLEncoding.EncodeToString(raw[:])
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue // another process created it; read theirs
		}
		if err != nil {
			return "", err
		}
		_, werr := f.WriteString(tok)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(path)
			return "", werr
		}
		return tok, nil
	}
	return "", errors.New("config: could not read or create control token")
}

func mustJSON(c Config) []byte {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil && !errors.Is(err, os.ErrInvalid) {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
