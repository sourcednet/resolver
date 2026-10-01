package resolver

import (
	"testing"
	"time"
)

func TestPolling(t *testing.T) {
	p := NewPolling(Config{Freshness: time.Minute})
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		maxAge time.Duration
		failed bool
		want   time.Duration
	}{
		{0, false, 5 * time.Minute},            // no max-age: the default interval
		{time.Second, false, 30 * time.Second}, // too short: MinPoll
		{10 * time.Minute, false, 10 * time.Minute},
		{24 * time.Hour, false, time.Hour}, // too long: MaxPoll
		{10 * time.Minute, true, 5 * time.Minute},
	} {
		if got := p.NextSync("x.test", now, tt.maxAge, tt.failed).Sub(now); got != tt.want {
			t.Errorf("max-age %v, failed %v: next in %v, want %v", tt.maxAge, tt.failed, got, tt.want)
		}
	}
	if !p.AcceptAnnounce("x.test", now) || p.AcceptAnnounce("x.test", now.Add(time.Second)) || !p.AcceptAnnounce("y.test", now) {
		t.Error("announces: want one per publisher every 10 s")
	}
	if !p.Fresh(now, now.Add(time.Minute)) || p.Fresh(now, now.Add(2*time.Minute)) {
		t.Error("freshness window not applied")
	}
}
