// Package httplog logs outgoing HTTP requests at debug level.
package httplog

import (
	"log/slog"
	"net/http"
	"time"
)

// Wrap returns a copy of client whose requests are logged to log.
func Wrap(client *http.Client, log *slog.Logger) *http.Client {
	c := *client
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = &transport{base: base, log: log}
	return &c
}

type transport struct {
	base http.RoundTripper
	log  *slog.Logger
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		t.log.Debug("http", "method", req.Method, "url", req.URL.String(), "err", err, "took", took)
		return nil, err
	}
	t.log.Debug("http", "method", req.Method, "url", req.URL.String(), "status", resp.StatusCode, "took", took)
	return resp, nil
}
