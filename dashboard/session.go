package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// sessionDuration is how long a login session stays valid.
const sessionDuration = 24 * time.Hour

const maxDashboardSessions = 4096

// sessionStore is an in-memory session token store. Each login creates a
// session token (returned as a cookie); logout deletes it. Sessions expire
// automatically after sessionDuration.
//
// The store uses a sync.RWMutex — read loads (every authenticated request)
// never block each other, only logout/login writes briefly hold the lock.
type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]sessionEntry
	// failed tracks repeated authentication failures by the peer address.
	// It is intentionally small and in-memory: the dashboard is an admin surface,
	// not a distributed identity provider.
	failed map[string]loginFailure
}

type loginFailure struct {
	count int
	until time.Time
}

type sessionEntry struct {
	username string
	expires  time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{
		sessions: make(map[string]sessionEntry),
		failed:   make(map[string]loginFailure),
	}
}

// cleanupExpired removes stale entries opportunistically. Session creation is
// rare compared with authenticated dashboard requests, so this avoids a
// permanent background goroutine while still bounding stale-session memory.
func (s *sessionStore) cleanupExpired(now time.Time) {
	for token, entry := range s.sessions {
		if now.After(entry.expires) {
			delete(s.sessions, token)
		}
	}
}

// create generates a new session token for username and stores it.
// Returns the opaque token string (32 hex chars = 16 bytes of entropy).
func (s *sessionStore) create(username string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	now := time.Now()
	s.mu.Lock()
	s.cleanupExpired(now)
	if len(s.sessions) >= maxDashboardSessions {
		// Expired entries were removed above. Never evict a live administrative
		// session to make room for another login; fail closed instead.
		return "", fmt.Errorf("dashboard session capacity reached")
	}
	s.sessions[token] = sessionEntry{
		username: username,
		expires:  now.Add(sessionDuration),
	}
	s.mu.Unlock()
	return token, nil
}

// valid checks whether token exists and has not expired. Returns the
// username and true if valid, "" and false otherwise.
func (s *sessionStore) valid(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	s.mu.RLock()
	entry, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expires) {
		s.mu.Lock()
		delete(s.sessions, token)
		s.mu.Unlock()
		return "", false
	}
	return entry.username, true
}

// destroy deletes a session token (logout).
func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// sessionCookieName is the name of the cookie that holds the session token.
const sessionCookieName = "breeze_dash_session"

// buildSessionCookie formats a Set-Cookie header value for the given token.
// HttpOnly prevents JS access; SameSite=Strict prevents CSRF on top-level navs;
// Path=/dashboard scopes the cookie to the dashboard subtree.
func buildSessionCookie(token, basePath string, maxAge int, secure bool) string {
	path := basePath
	if path == "" {
		path = "/dashboard"
	}
	flags := "; HttpOnly; SameSite=Strict"
	if secure {
		flags += "; Secure"
	}
	if maxAge <= 0 {
		// Expire immediately (logout).
		return sessionCookieName + "=" + token + "; Path=" + path + "; Max-Age=0" + flags
	}
	return sessionCookieName + "=" + token + "; Path=" + path + "; Max-Age=" + strconv.Itoa(maxAge) + flags
}

const (
	maxLoginFailures   = 8
	loginBlockDuration = 15 * time.Minute
)

func (s *sessionStore) allowLogin(peer string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.failed[peer]
	if !ok {
		return true
	}
	if !entry.until.IsZero() && now.Before(entry.until) {
		return false
	}
	if !entry.until.IsZero() {
		delete(s.failed, peer)
	}
	return true
}

func (s *sessionStore) recordLoginFailure(peer string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.failed[peer]
	entry.count++
	if entry.count >= maxLoginFailures {
		entry.until = now.Add(loginBlockDuration)
		entry.count = maxLoginFailures
	}
	s.failed[peer] = entry
}

func (s *sessionStore) clearLoginFailures(peer string) {
	s.mu.Lock()
	delete(s.failed, peer)
	s.mu.Unlock()
}
