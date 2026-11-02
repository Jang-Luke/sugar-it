package mesh

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const invitePrefix = "sugar-it1:"

type Invite struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Addrs   []string  `json:"addrs"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// Encode returns "sugar-it1:" followed by base64url (unpadded) JSON.
func (i Invite) Encode() string {
	b, _ := json.Marshal(i)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeInvite parses an invite string. Errors never echo the input, which
// carries a secret token.
func DecodeInvite(s string) (Invite, error) {
	var inv Invite
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), invitePrefix)
	if !ok {
		return inv, errors.New("invite: missing sugar-it1: prefix")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(rest, "="))
	if err != nil {
		return inv, errors.New("invite: bad encoding")
	}
	if err := json.Unmarshal(b, &inv); err != nil {
		return Invite{}, errors.New("invite: bad contents")
	}
	if inv.ID == "" || inv.Name == "" || len(inv.Addrs) == 0 || inv.Token == "" || inv.Expires.IsZero() {
		return Invite{}, errors.New("invite: missing field")
	}
	return inv, nil
}

// Tokens issues one-time pairing tokens held in memory only.
type Tokens struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]time.Time
}

func NewTokens(now func() time.Time) *Tokens {
	return &Tokens{now: now, m: map[string]time.Time{}}
}

// Issue returns a new 128-bit random token valid for ttl.
func (t *Tokens) Issue(ttl time.Duration) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	t.mu.Lock()
	defer t.mu.Unlock()
	t.purgeLocked()
	t.m[tok] = t.now().Add(ttl)
	return tok
}

// Consume reports whether token is valid and unexpired, and invalidates it.
func (t *Tokens) Consume(token string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.m[token]
	delete(t.m, token)
	t.purgeLocked()
	return ok && t.now().Before(exp)
}

func (t *Tokens) purgeLocked() {
	now := t.now()
	for k, exp := range t.m {
		if !now.Before(exp) {
			delete(t.m, k)
		}
	}
}
