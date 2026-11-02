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

	"sugarintegration/internal/fsutil"
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
		if err := fsutil.WriteAtomic(path, mustJSON(s.cur), 0o600); err != nil {
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
	if err := fsutil.WriteAtomic(filepath.Join(s.dir, configFile), mustJSON(next), 0o600); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// ControlToken returns the control API token, creating it (mode 0600) if
// absent. The token is written to a temp file and hard-linked into place, so a
// reader never sees a partial file and concurrent callers agree on one token.
// An empty file (left by a crash or an older version) is stale and replaced.
func ControlToken(dir string) (string, error) {
	path := filepath.Join(dir, tokenFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if tok, err := readToken(path); err != nil || tok != "" {
		return tok, err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw[:])
	tmp, err := os.CreateTemp(dir, tokenFile+".tmp*") // mode 0600
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(tok)
	if serr := tmp.Sync(); werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	for range 3 {
		err := os.Link(tmp.Name(), path)
		if err == nil {
			return tok, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		existing, rerr := readToken(path)
		if rerr != nil {
			return "", rerr
		}
		if existing != "" {
			return existing, nil // another caller won
		}
		// Stale empty file: replace it atomically.
		if err := os.Rename(tmp.Name(), path); err != nil {
			return "", err
		}
		return readToken(path)
	}
	return "", errors.New("config: could not create control token")
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

func mustJSON(c Config) []byte {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}
