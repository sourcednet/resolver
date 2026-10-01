// Package ratelimit bounds how many requests each client may make: a
// steady rate with short bursts (a token bucket per client).
package ratelimit

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limiter allows each client Rate requests per second on average, and up
// to Burst at once.
type Limiter struct {
	rate, burst float64
	now         func() time.Time

	mu      sync.Mutex
	clients map[string]*bucket
	calls   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter allowing rate requests per second per client, and
// bursts of up to burst.
func New(rate float64, burst int) *Limiter {
	return &Limiter{rate: rate, burst: float64(burst), now: time.Now, clients: map[string]*bucket{}}
}

// idleAfter is when a client's full bucket is forgotten, to keep memory
// bounded.
const idleAfter = 10 * time.Minute

// Allow reports whether client may make a request now, and if not, how
// long until it may.
func (l *Limiter) Allow(client string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.calls++
	if l.calls%1024 == 0 {
		for c, b := range l.clients {
			if now.Sub(b.last) > idleAfter {
				delete(l.clients, c)
			}
		}
	}
	b, ok := l.clients[client]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.clients[client] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// Handler limits requests to next per client, answering 429 Too Many
// Requests with a Retry-After header when a client goes over. ipHeader,
// if set, names a header holding the client's address, set by a trusted
// proxy in front (such as nginx's X-Real-IP); otherwise the connection's
// address is used.
func (l *Limiter) Handler(next http.Handler, ipHeader string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := l.Allow(clientIP(r, ipHeader))
		if !ok {
			w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(wait.Seconds()))))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request, header string) string {
	if header != "" {
		if v := strings.TrimSpace(r.Header.Get(header)); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
