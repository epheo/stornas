package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeSource struct{ users map[string][2]string } // name -> [role, password]

func (f *fakeSource) Lookup(_ context.Context, username string) (User, string, error) {
	u, ok := f.users[username]
	if !ok {
		return User{}, "", fmt.Errorf("no such user")
	}
	return User{Name: username, Role: u[0]}, u[1], nil
}

func newTestManager() *Manager {
	m := NewManager(&fakeSource{users: map[string][2]string{
		"admin": {"admin", "hunter2"},
	}})
	return m
}

func login(t *testing.T, m *Manager, user, pass string) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)
	r := httptest.NewRequest("POST", "/api/v1/login", strings.NewReader(body))
	w := httptest.NewRecorder()
	m.Login(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.Value != "" {
			return w, c
		}
	}
	return w, nil
}

func TestLoginAndSession(t *testing.T) {
	m := newTestManager()
	w, cookie := login(t, m, "admin", "hunter2")
	if w.Code != http.StatusOK || cookie == nil {
		t.Fatalf("login: code=%d cookie=%v", w.Code, cookie)
	}
	if !cookie.HttpOnly {
		t.Fatal("cookie must be HttpOnly")
	}

	r := httptest.NewRequest("GET", "/api/v1/session", nil)
	r.AddCookie(cookie)
	sw := httptest.NewRecorder()
	m.Session(sw, r)
	if sw.Code != http.StatusOK || !strings.Contains(sw.Body.String(), `"role":"admin"`) {
		t.Fatalf("session: code=%d body=%s", sw.Code, sw.Body.String())
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	m := newTestManager()
	start := time.Now()
	w, cookie := login(t, m, "admin", "wrong")
	if w.Code != http.StatusUnauthorized || cookie != nil {
		t.Fatalf("code=%d cookie=%v", w.Code, cookie)
	}
	if time.Since(start) < failDelay {
		t.Fatal("failed login returned before the delay")
	}
}

func TestRequireGates(t *testing.T) {
	m := newTestManager()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	w := httptest.NewRecorder()
	m.Require(next).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/state", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie: code=%d", w.Code)
	}

	_, cookie := login(t, m, "admin", "hunter2")
	r := httptest.NewRequest("GET", "/api/v1/state", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	m.Require(next).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("with cookie: code=%d", w.Code)
	}
}

func TestLogoutRevokes(t *testing.T) {
	m := newTestManager()
	_, cookie := login(t, m, "admin", "hunter2")

	r := httptest.NewRequest("POST", "/api/v1/logout", nil)
	r.AddCookie(cookie)
	m.Logout(httptest.NewRecorder(), r)

	r = httptest.NewRequest("GET", "/api/v1/session", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	m.Session(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session still valid: code=%d", w.Code)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	m := newTestManager()
	m.ttl = -time.Second // everything is born expired
	_, cookie := login(t, m, "admin", "hunter2")
	if cookie == nil {
		t.Fatal("no cookie")
	}
	r := httptest.NewRequest("GET", "/api/v1/session", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	m.Session(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired session accepted: code=%d", w.Code)
	}
}
