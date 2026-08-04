// Package auth is the appliance session layer: LocalUsers checked against
// their password Secrets, server-side sessions behind an HttpOnly cookie.
// No Kubernetes identity is involved; appliance users have no kubeconfig,
// and the server's own ServiceAccount stays the only cluster principal
// until mutation endpoints need per-role scoping.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const cookieName = "stornas_session"

// User is an authenticated appliance identity.
type User struct {
	Name string `json:"name"`
	Role string `json:"role"` // admin | viewer
}

// Source looks up one user and its expected password. Password comparison
// stays in the Manager so every Source gets constant-time treatment.
type Source interface {
	Lookup(ctx context.Context, username string) (User, string, error)
}

// Manager owns sessions. Sliding expiry: any authenticated request renews.
type Manager struct {
	src Source
	ttl time.Duration

	mu       sync.Mutex
	sessions map[string]session
}

type session struct {
	user    User
	expires time.Time
}

func NewManager(src Source) *Manager {
	return &Manager{src: src, ttl: 12 * time.Hour, sessions: map[string]session{}}
}

// failDelay slows credential guessing without a rate-limit table; the
// constant-time compare handles the timing side.
const failDelay = 400 * time.Millisecond

func (m *Manager) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user, pw, err := m.src.Lookup(r.Context(), req.Username)
	ok := err == nil && pw != "" &&
		subtle.ConstantTimeCompare([]byte(pw), []byte(req.Password)) == 1
	if !ok {
		time.Sleep(failDelay)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	tok := rand.Text() + rand.Text()
	m.mu.Lock()
	m.gcLocked()
	m.sessions[tok] = session{user: user, expires: time.Now().Add(m.ttl)}
	m.mu.Unlock()

	http.SetCookie(w, m.cookie(r, tok, int(m.ttl.Seconds())))
	writeJSON(w, user)
}

func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		m.mu.Lock()
		delete(m.sessions, c.Value)
		m.mu.Unlock()
	}
	http.SetCookie(w, m.cookie(r, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

// Session reports the caller's identity; the UI's login gate.
func (m *Manager) Session(w http.ResponseWriter, r *http.Request) {
	user, ok := m.userFor(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	writeJSON(w, user)
}

// Require gates a handler behind a valid session.
func (m *Manager) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := m.userFor(r); !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRole gates a handler behind a session carrying the role; admin
// implies every role.
func (m *Manager) RequireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := m.userFor(r)
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if user.Role != role && user.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Manager) userFor(r *http.Request) (User, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return User{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[c.Value]
	if !ok || time.Now().After(s.expires) {
		delete(m.sessions, c.Value)
		return User{}, false
	}
	s.expires = time.Now().Add(m.ttl)
	m.sessions[c.Value] = s
	return s.user, true
}

// gcLocked drops expired sessions on login, bounding the map by active
// users rather than process lifetime.
func (m *Manager) gcLocked() {
	now := time.Now()
	for k, s := range m.sessions {
		if now.After(s.expires) {
			delete(m.sessions, k)
		}
	}
}

func (m *Manager) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
