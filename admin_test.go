package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAdminAccessAndLogout(t *testing.T) {
	const password = "fictional-test-password"
	var hash strings.Builder
	if err := printAdminHash(strings.NewReader(password), &hash); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := mountAdmin(mux, nil, votingEvent{}, strings.TrimSpace(hash.String()), "127.0.0.1:8081", false); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, password, origin string, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"password": {password}}
		r := httptest.NewRequest(method, "https://vote.example"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d, want %d", method, path, w.Code, want)
		}
		return w
	}

	w := request("GET", "/admin", "", "", nil, http.StatusSeeOther)
	if w.Header().Get("Location") != "/admin/login" {
		t.Fatal("expected login redirect")
	}
	request("POST", "/admin/logout", "", "", nil, http.StatusUnauthorized)
	request("GET", "/admin/events/1/participants/1/photo", "", "", nil, http.StatusSeeOther)
	request("POST", "/admin/events/1/participants/new", "", "", nil, http.StatusUnauthorized)
	request("POST", "/admin/voting", "", "", nil, http.StatusUnauthorized)
	request("GET", "/admin/results", "", "", nil, http.StatusSeeOther)
	request("GET", "/admin/results/export", "", "", nil, http.StatusSeeOther)
	request("POST", "/admin/login", password, "https://other.example", nil, http.StatusForbidden)
	request("POST", "/admin/login", "wrong", "", nil, http.StatusUnauthorized)
	w = request("POST", "/admin/login", password, "https://vote.example", nil, http.StatusSeeOther)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/admin" {
		t.Fatal("expected protected admin cookie")
	}
	cookie := cookies[0]
	request("GET", "/admin/login", "", "", cookie, http.StatusSeeOther)
	request("POST", "/admin/logout", "", "https://other.example", cookie, http.StatusForbidden)
	request("GET", "/admin/login", "", "", cookie, http.StatusSeeOther)
	request("POST", "/admin/logout", "", "https://vote.example", cookie, http.StatusSeeOther)
	request("GET", "/admin", "", "", cookie, http.StatusSeeOther)
	for i := 2; i < adminAttempts; i++ {
		request("POST", "/admin/login", "wrong", "", nil, http.StatusUnauthorized)
	}
	request("POST", "/admin/login", password, "", nil, http.StatusTooManyRequests)
}
