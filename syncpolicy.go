package resolver

import (
	"sync"
	"time"
)

// A SyncPolicy decides when the resolver checks which publisher. Syncing
// itself (fetch what's new, verify, store, record changes) is the same
// whatever the policy.
type SyncPolicy interface {
	// NextSync returns when to sync a publisher again after a sync that
	// ended at now. maxAge is the max-age its manifest was served with (0
	// if none, or not modified); failed reports a failed sync.
	NextSync(publisher string, now time.Time, maxAge time.Duration, failed bool) time.Time
	// AcceptAnnounce reports whether a publisher's announce at now should
	// start a sync, or is too soon after the last one.
	AcceptAnnounce(publisher string, now time.Time) bool
	// Fresh reports whether a page's current record, last confirmed with
	// the publisher at checked, can still be used at now without asking.
	Fresh(checked, now time.Time) bool
}

// Polling is the default sync policy: poll each publisher as often as its
// manifest's max-age says, within bounds; accept announces at most once
// per AnnounceEvery per publisher; trust a page's record for Freshness.
type Polling struct {
	// Interval applies when a publisher sends no max-age, or a sync fails.
	Interval time.Duration
	// MinPoll and MaxPoll bound the max-age a publisher sends.
	MinPoll, MaxPoll time.Duration
	// AnnounceEvery is the least time between announces a publisher may send.
	AnnounceEvery time.Duration
	// Freshness is how long a page's current record is trusted. Zero
	// rechecks on every request.
	Freshness time.Duration

	mu        sync.Mutex
	announced map[string]time.Time
}

// NewPolling returns the polling policy for cfg's settings, with defaults
// for those not set.
func NewPolling(cfg Config) *Polling {
	p := &Polling{
		Interval: cfg.PollInterval, MinPoll: cfg.MinPoll, MaxPoll: cfg.MaxPoll,
		AnnounceEvery: cfg.AnnounceEvery, Freshness: cfg.Freshness,
		announced: map[string]time.Time{},
	}
	if p.Interval == 0 {
		p.Interval = 5 * time.Minute
	}
	if p.MinPoll == 0 {
		p.MinPoll = 30 * time.Second
	}
	if p.MaxPoll == 0 {
		p.MaxPoll = time.Hour
	}
	if p.AnnounceEvery == 0 {
		p.AnnounceEvery = 10 * time.Second
	}
	return p
}

// NextSync implements SyncPolicy.
func (p *Polling) NextSync(_ string, now time.Time, maxAge time.Duration, failed bool) time.Time {
	if failed || maxAge <= 0 {
		return now.Add(p.Interval)
	}
	return now.Add(min(max(maxAge, p.MinPoll), p.MaxPoll))
}

// AcceptAnnounce implements SyncPolicy.
func (p *Polling) AcceptAnnounce(publisher string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.announced[publisher]; ok && now.Sub(last) < p.AnnounceEvery {
		return false
	}
	p.announced[publisher] = now
	return true
}

// Fresh implements SyncPolicy.
func (p *Polling) Fresh(checked, now time.Time) bool {
	return p.Freshness > 0 && now.Sub(checked) <= p.Freshness
}
