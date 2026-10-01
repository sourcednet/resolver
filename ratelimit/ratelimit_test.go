package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	l := New(2, 3)
	l.now = func() time.Time { return now }
	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d of a burst of 3 refused", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("over the burst: allowed %v, wait %v; want refused, 500ms", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another client was limited too")
	}
	now = now.Add(500 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a token should have come back after 1/rate")
	}
}

func TestHandler(t *testing.T) {
	h := New(1, 1).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}), "X-Real-IP")
	get := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/sourced/v1/search?q=x", nil)
		r.Header.Set("X-Real-IP", ip)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := get("203.0.113.1"); w.Code != http.StatusOK {
		t.Fatalf("first request: %d", w.Code)
	}
	if w := get("203.0.113.1"); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("second request: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := get("203.0.113.2"); w.Code != http.StatusOK {
		t.Fatalf("another client behind the same proxy: %d", w.Code)
	}
}
