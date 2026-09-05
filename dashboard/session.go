package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"time"
)

// sessionDuration is how long a login session stays valid.
const sessionDuration = 24 * time.Hour

// sessionStore is an in-memory session token store. Each login creates a
// session token (returned as a cookie); logout deletes it. Sessions expire
// automatically after sessionDuration.
//
// The store uses a sync.RWMutex — read loads (every authenticated request)
// never block each other, only logout/login writes briefly hold the lock.
type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]sessionEntry
}

type sessionEntry struct {
	username string
	expires  time.Time
}

func newSessionStore() *sessionStore {
	s := &sessionStore{
		sessions: make(map[string]sessionEntry),
	}
	// Start a background goroutine to periodically clean expired sessions.
	go s.cleanupLoop()
	return s
}

// cleanupLoop runs in the background and removes expired sessions every 10 minutes.
func (s *sessionStore) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.cleanup()
	}
}

// cleanup removes all expired sessions. Called periodically by cleanupLoop.
func (s *sessionStore) cleanup() {
	now := time.Now()
	s.mu.Lock()
	for token, entry := range s.sessions {
		if now.After(entry.expires) {
			delete(s.sessions, token)
		}
	}
	s.mu.Unlock()
}

// create generates a new session token for username and stores it.
// Returns the opaque token string (32 hex chars = 16 bytes of entropy).
func (s *sessionStore) create(username string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	s.mu.Lock()
	s.sessions[token] = sessionEntry{
		username: username,
		expires:  time.Now().Add(sessionDuration),
	}
	s.mu.Unlock()
	return token
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
// HttpOnly prevents JS access; SameSite=Lax prevents CSRF on top-level navs;
// Path=/dashboard scopes the cookie to the dashboard subtree.
func buildSessionCookie(token, basePath string, maxAge int) string {
	path := basePath
	if path == "" {
		path = "/dashboard"
	}
	if maxAge <= 0 {
		// Expire immediately (logout).
		return sessionCookieName + "=" + token + "; Path=" + path + "; Max-Age=0; HttpOnly; SameSite=Lax"
	}
	return sessionCookieName + "=" + token + "; Path=" + path + "; Max-Age=" + strconv.Itoa(
		maxAge,
	) + "; HttpOnly; SameSite=Lax"
}
