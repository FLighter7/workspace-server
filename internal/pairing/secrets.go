package pairing

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"
)

const secretTTL = 10 * time.Minute

// Secrets holds the single live one-time pairing secret, in memory only.
type Secrets struct {
	now func() time.Time

	mu      sync.Mutex
	secret  string
	expires time.Time
}

// NewSecrets returns an empty Secrets that reads time from now.
func NewSecrets(now func() time.Time) *Secrets {
	return &Secrets{now: now}
}

// Issue creates a new secret valid for 10 minutes and revokes the previous one.
func (s *Secrets) Issue() (secret string, expires time.Time) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail on supported platforms.
		panic("pairing: read random bytes: " + err.Error())
	}
	secret = base64.RawURLEncoding.EncodeToString(buf)
	expires = s.now().Add(secretTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.secret, s.expires = secret, expires
	return secret, expires
}

// Check reports whether secret is the live, unexpired secret. It does not
// consume it.
func (s *Secrets) Check(secret string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validLocked(secret)
}

// Consume reports whether secret was valid and, if so, removes it so that it
// can never succeed again.
func (s *Secrets) Consume(secret string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.validLocked(secret) {
		return false
	}
	s.secret, s.expires = "", time.Time{}
	return true
}

func (s *Secrets) validLocked(secret string) bool {
	if secret == "" || s.secret == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(s.secret)) != 1 {
		return false
	}
	return s.now().Before(s.expires)
}
