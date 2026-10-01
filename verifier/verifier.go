// Package verifier implements the spec's verification procedures: fetching
// a page's signed content, resolving a citation to its current state, and
// checking a passage. Resolvers and validating clients both build on it.
//
// A Verifier always fetches keys directly from the publisher and checks
// every signature and hash itself.
package verifier

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/core/htmllink"
)

// Verification is the outcome of checking a page or citation.
type Verification string

const (
	Verified Verification = "verified"
	// NotParticipating means the site publishes no signed content at all.
	NotParticipating Verification = "not-participating"
	// Unlisted means the publisher signs content, but not this URL.
	Unlisted Verification = "unlisted"
	Failed   Verification = "failed"
)

// ErrNotFound means the publisher answered 404 or 410.
var ErrNotFound = errors.New("not found")

// maxBody caps how much of any response is read.
const maxBody = 16 << 20

// Verifier fetches and verifies publisher content. It is safe for
// concurrent use.
type Verifier struct {
	http   *http.Client
	keyTTL time.Duration
	now    func() time.Time
	obs    Observer
	cache  Cache
	log    *slog.Logger

	mu   sync.Mutex
	keys map[string]cachedKeys
	seen map[string]time.Time // newest manifest generated_at per publisher
}

type cachedKeys struct {
	ks      *core.KeySet
	fetched time.Time
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithKeyTTL sets how long a publisher's keys.json is cached. Default 5 minutes.
func WithKeyTTL(d time.Duration) Option { return func(v *Verifier) { v.keyTTL = d } }

// WithClock sets the time source, for tests.
func WithClock(now func() time.Time) Option { return func(v *Verifier) { v.now = now } }

// WithLogger logs where keys, records, and bundles come from (debug) and
// what fails verification (warn).
func WithLogger(l *slog.Logger) Option { return func(v *Verifier) { v.log = l } }

// Observer sees every record and bundle a Verifier has verified. A resolver
// uses it to keep what it has checked.
type Observer interface {
	ObserveKeys(publisher string, raw []byte, at time.Time)
	ObserveRecord(raw []byte, r *core.Record)
	ObserveBundle(r *core.Record, b *core.Bundle)
}

// WithObserver reports verified records and bundles to o.
func WithObserver(o Observer) Option { return func(v *Verifier) { v.obs = o } }

// Cache holds records and bundles fetched earlier. They never change, so a
// Verifier checks the cache before the network. Cached data is verified on
// every use like anything else; if it fails, the Verifier asks the publisher.
//
// Cached keys are used only when the publisher can't be reached, and only
// up to MaxKeyStaleness after they were fetched.
type Cache interface {
	CachedKeys(publisher string) (raw []byte, fetched time.Time, ok bool)
	CachedRecord(id string) ([]byte, bool)
	CachedBundle(recordID string) (*core.Bundle, bool)
}

// MaxKeyStaleness is how long after fetching them a Verifier still uses a
// publisher's keys when the publisher can't be reached (serve-stale). A key
// revoked in that time is still trusted until the publisher is reachable.
const MaxKeyStaleness = 24 * time.Hour

// WithCache makes the Verifier read records and bundles from c first.
func WithCache(c Cache) Option { return func(v *Verifier) { v.cache = c } }

// New returns a Verifier using client for every request. A nil client uses
// http.DefaultClient.
func New(client *http.Client, opts ...Option) *Verifier {
	if client == nil {
		client = http.DefaultClient
	}
	v := &Verifier{
		http:   client,
		keyTTL: 5 * time.Minute,
		now:    time.Now,
		keys:   map[string]cachedKeys{},
		seen:   map[string]time.Time{},
		log:    slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

// HTTPClient returns a client with a request timeout. If caFile is set, its
// PEM certificates are trusted in addition to the system roots: a dev-only
// setting for local test networks with their own certificate authority.
func HTTPClient(caFile string, timeout time.Duration) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no PEM certificates found", caFile)
		}
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = new(tls.Config)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

// Chunk is a verified passage with its citation.
type Chunk struct {
	ID      string   `json:"id"`
	Section []string `json:"section,omitempty"`
	Text    string   `json:"text"`
	Cite    string   `json:"cite"`
}

// Page is the verified content of a page.
type Page struct {
	Verification Verification `json:"verification"`
	Reason       core.Reason  `json:"reason,omitempty"`
	Detail       string       `json:"detail,omitempty"`
	Publisher    string       `json:"publisher,omitempty"`
	URL          string       `json:"url"`
	Title        string       `json:"title,omitempty"`
	PublishedAt  time.Time    `json:"published_at,omitzero"`
	Language     string       `json:"language,omitempty"`
	State        core.State   `json:"state,omitempty"`
	Record       string       `json:"record,omitempty"`
	Chunks       []Chunk      `json:"chunks,omitempty"`
	// Listed holds some of the publisher's signed pages when this URL is
	// unlisted, so the caller can pick the right one.
	Listed []string `json:"listed,omitempty"`
}

// maxListed caps how many of a publisher's pages an unlisted answer names.
const maxListed = 25

// NotListedError means a publisher takes part but its manifest doesn't
// list the page. It matches ErrNoRecord with errors.Is.
type NotListedError struct {
	Publisher string
	Pages     []string // some of the publisher's signed pages, sorted
}

func (e *NotListedError) Error() string {
	return e.Publisher + " publishes signed content, but not for this page"
}

// Is makes a NotListedError count as ErrNoRecord.
func (e *NotListedError) Is(target error) bool { return target == ErrNoRecord }

// Unlisted fills p in as a page its publisher doesn't list.
func (e *NotListedError) Unlisted(p *Page) {
	p.Verification, p.Detail, p.Listed = Unlisted, e.Error(), e.Pages
}

// failure turns a verification error into a result, or returns it as an
// error if it isn't one (network trouble, for example).
func failure(err error) (core.Reason, string, error) {
	if r := core.ReasonOf(err); r != "" {
		return r, err.Error(), nil
	}
	return "", "", err
}

// Fetch returns the verified content of the page at pageURL, following the
// spec: the page's Link header, then its <link> element, then the manifest.
// A page whose publisher doesn't take part is NotParticipating. Content that
// fails a check is Failed with a reason. Errors are for failures to reach
// the publisher at all.
func (v *Verifier) Fetch(ctx context.Context, pageURL string) (*Page, error) {
	pub, err := publisherOf(pageURL)
	if err != nil {
		return nil, err
	}
	p := &Page{URL: pageURL, Publisher: pub}
	fail := func(err error) (*Page, error) {
		r, d, err := failure(err)
		if err != nil {
			return nil, err
		}
		p.Verification, p.Reason, p.Detail = Failed, r, d
		return p, nil
	}
	id, err := v.currentRecordID(ctx, pageURL)
	var nl *NotListedError
	switch {
	case errors.As(err, &nl):
		nl.Unlisted(p)
		return p, nil
	case errors.Is(err, ErrNoRecord):
		p.Verification = NotParticipating
		return p, nil
	}
	if err != nil {
		return fail(err)
	}
	return v.FetchRecord(ctx, pageURL, id)
}

// FetchRecord returns the verified content of pageURL as of a known record,
// skipping the lookup of the current one. The record must be for pageURL,
// unless the publisher's manifest maps pageURL to it (a moved page).
func (v *Verifier) FetchRecord(ctx context.Context, pageURL, id string) (*Page, error) {
	pub, err := publisherOf(pageURL)
	if err != nil {
		return nil, err
	}
	p := &Page{URL: pageURL, Publisher: pub}
	fail := func(err error) (*Page, error) {
		r, d, err := failure(err)
		if err != nil {
			return nil, err
		}
		p.Verification, p.Reason, p.Detail = Failed, r, d
		return p, nil
	}
	r, err := v.record(ctx, pub, id)
	if err != nil {
		return fail(err)
	}
	if normalizeURL(r.URL) != normalizeURL(pageURL) {
		// A moved page: allowed only if the manifest maps this URL to the record.
		if mid, err := v.manifestEntry(ctx, pub, pageURL); err != nil || mid != r.ID {
			return fail(&core.VerifyError{Reason: core.ReasonURLMismatch, Detail: fmt.Sprintf("record is for %s, not %s", r.URL, pageURL)})
		}
	}
	p.Title, p.PublishedAt, p.Language, p.Record = r.Title, r.PublishedAt, r.Language, r.ID
	p.State = core.StateCurrent
	if r.Change == core.ChangeWithdrawal {
		p.Verification, p.State = Verified, core.StateWithdrawn
		return p, nil
	}
	b, err := v.bundle(ctx, pub, r)
	if err != nil {
		return fail(err)
	}
	p.Chunks = chunksOf(r, b)
	p.Verification = Verified
	return p, nil
}

func chunksOf(r *core.Record, b *core.Bundle) []Chunk {
	out := make([]Chunk, len(b.Chunks))
	for i, c := range b.Chunks {
		out[i] = Chunk{
			ID:      c.ID,
			Section: r.Chunks[i].Section,
			Text:    c.Text,
			Cite:    core.Citation{Publisher: r.Publisher, Record: r.ID, Chunk: c.ID}.String(),
		}
	}
	return out
}

// ErrNoRecord means the page has no record: the publisher doesn't take part,
// or doesn't list this page.
var ErrNoRecord = errors.New("no sourced record for this page")

// CurrentRecordID finds the page's current record: from the Link header of
// a HEAD request, then the page's <link> element, then the manifest. It
// always asks the publisher live. ErrNoRecord means there is none.
func (v *Verifier) CurrentRecordID(ctx context.Context, pageURL string) (string, error) {
	return v.currentRecordID(ctx, pageURL)
}

func (v *Verifier) currentRecordID(ctx context.Context, pageURL string) (string, error) {
	pub, err := publisherOf(pageURL)
	if err != nil {
		return "", err
	}
	_, hdr, headErr := v.get(ctx, http.MethodHead, pageURL)
	if headErr == nil {
		if href := htmllink.FromHeader(hdr, pageURL); href != "" {
			return recordIDFromURL(href, pub)
		}
		body, _, err := v.get(ctx, http.MethodGet, pageURL)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
		if href := htmllink.FromHTML(body, pageURL); href != "" {
			return recordIDFromURL(href, pub)
		}
	} else if !errors.Is(headErr, ErrNotFound) {
		return "", headErr
	}

	id, err := v.manifestEntry(ctx, pub, pageURL)
	if errors.Is(err, ErrNotFound) {
		return "", ErrNoRecord
	}
	return id, err
}

// ErrNotModified means a conditional manifest request found no change.
var ErrNotModified = errors.New("manifest not modified")

// ManifestResult is a verified manifest and its caching information.
type ManifestResult struct {
	Manifest *core.Manifest
	ETag     string
	MaxAge   time.Duration // from Cache-Control; zero if absent
}

// Manifest fetches and verifies a publisher's manifest. With an etag, it
// asks conditionally and returns ErrNotModified if nothing changed. A
// manifest older than one this Verifier has already seen is rejected as
// stale (rollback protection).
func (v *Verifier) Manifest(ctx context.Context, pub, etag string) (*ManifestResult, error) {
	ks, err := v.keySet(ctx, pub, false)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	if etag != "" {
		hdr.Set("If-None-Match", etag)
	}
	raw, h, status, err := v.request(ctx, http.MethodGet, core.ManifestURL(pub), hdr)
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNotModified:
		v.log.Debug("manifest", "publisher", pub, "status", "not modified")
		return nil, ErrNotModified
	case status == http.StatusNotFound || status == http.StatusGone:
		return nil, ErrNotFound
	case status != http.StatusOK:
		return nil, fmt.Errorf("GET %s: %s", core.ManifestURL(pub), http.StatusText(status))
	}
	v.mu.Lock()
	last := v.seen[pub]
	v.mu.Unlock()
	m, err := core.VerifyManifest(raw, ks, pub, last)
	if err != nil {
		v.log.Warn("manifest rejected", "publisher", pub, "err", err)
		return nil, err
	}
	v.log.Debug("manifest", "publisher", pub, "status", "fetched", "entries", len(m.Entries), "generated_at", m.GeneratedAt.Format(time.RFC3339))
	v.mu.Lock()
	if m.GeneratedAt.After(v.seen[pub]) {
		v.seen[pub] = m.GeneratedAt
	}
	v.mu.Unlock()
	if m.Shards != "" {
		return nil, errors.New("sharded manifests are not supported yet")
	}
	return &ManifestResult{Manifest: m, ETag: h.Get("ETag"), MaxAge: maxAge(h)}, nil
}

// manifestEntry returns the record the publisher's manifest lists for pageURL.
func (v *Verifier) manifestEntry(ctx context.Context, pub, pageURL string) (string, error) {
	res, err := v.Manifest(ctx, pub, "")
	if err != nil {
		return "", err
	}
	want := normalizeURL(pageURL)
	for u, id := range res.Manifest.Entries {
		if normalizeURL(u) == want {
			return id, nil
		}
	}
	nl := &NotListedError{Publisher: pub}
	for u := range res.Manifest.Entries {
		nl.Pages = append(nl.Pages, u)
	}
	slices.Sort(nl.Pages)
	if len(nl.Pages) > maxListed {
		nl.Pages = nl.Pages[:maxListed]
	}
	return "", nl
}

func maxAge(h http.Header) time.Duration {
	for _, d := range strings.Split(h.Get("Cache-Control"), ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(d), "max-age="); ok {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return 0
}

// keySet returns the publisher's keys, fetched directly from the publisher
// and cached for the key TTL. refresh skips the cache. If the publisher
// can't be reached, the last keys fetched are used for up to
// MaxKeyStaleness (serve-stale).
func (v *Verifier) keySet(ctx context.Context, pub string, refresh bool) (*core.KeySet, error) {
	now := v.now()
	v.mu.Lock()
	c, ok := v.keys[pub]
	v.mu.Unlock()
	if ok && !refresh && now.Sub(c.fetched) < v.keyTTL {
		v.log.Debug("keys", "publisher", pub, "source", "cache")
		return c.ks, nil
	}
	raw, _, err := v.get(ctx, http.MethodGet, core.KeysURL(pub))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if ok && now.Sub(c.fetched) <= MaxKeyStaleness {
			v.log.Warn("keys", "publisher", pub, "source", "stale", "fetched", c.fetched.Format(time.RFC3339), "err", err)
			return c.ks, nil
		}
		if v.cache != nil {
			if raw, at, ok := v.cache.CachedKeys(pub); ok && now.Sub(at) <= MaxKeyStaleness {
				if ks, perr := core.ParseKeySet(raw, pub); perr == nil {
					v.mu.Lock()
					v.keys[pub] = cachedKeys{ks: ks, fetched: at}
					v.mu.Unlock()
					v.log.Warn("keys", "publisher", pub, "source", "stale (stored)", "fetched", at.Format(time.RFC3339), "err", err)
					return ks, nil
				}
			}
		}
		return nil, err
	}
	ks, err := core.ParseKeySet(raw, pub)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	v.keys[pub] = cachedKeys{ks: ks, fetched: now}
	v.mu.Unlock()
	v.log.Debug("keys", "publisher", pub, "source", "network", "keys", len(ks.Keys), "refresh", refresh)
	if v.obs != nil {
		v.obs.ObserveKeys(pub, raw, now)
	}
	return ks, nil
}

// record fetches and verifies a record, from the cache if it has a copy
// that verifies. If it names a key the cached key set doesn't know, as after
// a key rotation, the keys are fetched again once.
func (v *Verifier) record(ctx context.Context, pub, id string) (*core.Record, error) {
	if v.cache != nil {
		if raw, ok := v.cache.CachedRecord(id); ok {
			r, err := v.verifyRecord(ctx, pub, id, raw)
			if err == nil {
				v.log.Debug("record", "id", short(id), "source", "cache")
				return r, nil
			}
			// The publisher may have re-signed it (key revocation): ask again.
			v.log.Info("cached record no longer verifies, refetching", "id", short(id), "err", err)
		}
	}
	u, err := core.RecordURL(pub, id)
	if err != nil {
		return nil, err
	}
	raw, _, err := v.get(ctx, http.MethodGet, u)
	if err != nil {
		return nil, err
	}
	r, err := v.verifyRecord(ctx, pub, id, raw)
	if err != nil {
		v.log.Warn("record rejected", "publisher", pub, "id", short(id), "err", err)
		return nil, err
	}
	v.log.Debug("record", "id", short(id), "source", "network", "url", r.URL, "change", string(r.Change))
	if v.obs != nil {
		v.obs.ObserveRecord(raw, r)
	}
	return r, nil
}

func (v *Verifier) verifyRecord(ctx context.Context, pub, id string, raw []byte) (*core.Record, error) {
	ks, err := v.keySet(ctx, pub, false)
	if err != nil {
		return nil, err
	}
	r, err := core.VerifyRecord(raw, ks, pub)
	if core.ReasonOf(err) == core.ReasonUnknownKey {
		if ks, err = v.keySet(ctx, pub, true); err != nil {
			return nil, err
		}
		r, err = core.VerifyRecord(raw, ks, pub)
	}
	if err != nil {
		return nil, err
	}
	if r.ID != id {
		return nil, &core.VerifyError{Reason: core.ReasonHashMismatch, Detail: fmt.Sprintf("asked for record %s, got %s", id, r.ID)}
	}
	return r, nil
}

// Record fetches a record from its publisher and verifies it.
func (v *Verifier) Record(ctx context.Context, pub, id string) (*core.Record, error) {
	return v.record(ctx, pub, id)
}

// Bundle fetches a verified record's bundle and verifies it.
func (v *Verifier) Bundle(ctx context.Context, r *core.Record) (*core.Bundle, error) {
	return v.bundle(ctx, r.Publisher, r)
}

func (v *Verifier) bundle(ctx context.Context, pub string, r *core.Record) (*core.Bundle, error) {
	if v.cache != nil {
		if b, ok := v.cache.CachedBundle(r.ID); ok && core.VerifyBundle(r, b) == nil {
			v.log.Debug("bundle", "record", short(r.ID), "source", "cache", "chunks", len(b.Chunks))
			return b, nil
		}
	}
	u, err := core.BundleURL(pub, r.ID)
	if err != nil {
		return nil, err
	}
	raw, _, err := v.get(ctx, http.MethodGet, u)
	if err != nil {
		return nil, err
	}
	var b core.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, &core.VerifyError{Reason: core.ReasonMalformed, Detail: "bundle: " + err.Error()}
	}
	if err := core.VerifyBundle(r, &b); err != nil {
		v.log.Warn("bundle rejected", "record", short(r.ID), "err", err)
		return nil, err
	}
	v.log.Debug("bundle", "record", short(r.ID), "source", "network", "chunks", len(b.Chunks))
	if v.obs != nil {
		v.obs.ObserveBundle(r, &b)
	}
	return &b, nil
}

// get performs a request and treats anything but 200 as an error;
// 404 and 410 are ErrNotFound.
func (v *Verifier) get(ctx context.Context, method, rawURL string) ([]byte, http.Header, error) {
	b, h, status, err := v.request(ctx, method, rawURL, nil)
	switch {
	case err != nil:
		return nil, nil, err
	case status == http.StatusNotFound || status == http.StatusGone:
		return nil, h, ErrNotFound
	case status != http.StatusOK:
		return nil, h, fmt.Errorf("%s %s: %d %s", method, rawURL, status, http.StatusText(status))
	}
	return b, h, nil
}

// request performs one request and returns the body of a 200 response.
// Only https URLs are allowed, and redirects off the host are refused.
func (v *Verifier) request(ctx context.Context, method, rawURL string, hdr http.Header) ([]byte, http.Header, int, error) {
	if !strings.HasPrefix(rawURL, "https://") {
		return nil, nil, 0, fmt.Errorf("refusing non-https URL %s", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	if resp.Request != nil && !strings.EqualFold(resp.Request.URL.Hostname(), req.URL.Hostname()) {
		return nil, nil, 0, fmt.Errorf("%s redirected to another host (%s)", rawURL, resp.Request.URL.Host)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.Header, resp.StatusCode, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return b, resp.Header, resp.StatusCode, err
}

// short shortens an ID for logs.
func short(id string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(id, core.RecordIDPrefix), core.ChunkIDPrefix)
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

func publisherOf(pageURL string) (string, error) {
	u, err := url.Parse(pageURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" {
		return "", fmt.Errorf("%s: want an https URL on a domain", pageURL)
	}
	return strings.ToLower(u.Hostname()), nil
}

// recordIDFromURL turns a record link into a record ID, insisting the record
// lives on the publisher's own domain.
func recordIDFromURL(href, pub string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Hostname(), pub) {
		return "", &core.VerifyError{Reason: core.ReasonURLMismatch, Detail: fmt.Sprintf("record link points to %s, not %s", u.Host, pub)}
	}
	name, ok := strings.CutPrefix(u.Path, core.WellKnownPath+"records/")
	h, ok2 := strings.CutSuffix(name, ".json")
	id := core.RecordIDPrefix + h
	if _, err := core.IDHex(id, core.RecordIDPrefix); !ok || !ok2 || err != nil {
		return "", &core.VerifyError{Reason: core.ReasonMalformed, Detail: "record link " + href + " is not a record URL"}
	}
	return id, nil
}

// normalizeURL lowercases the scheme and host and drops the fragment, per
// the spec's URL matching rule.
func normalizeURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.Scheme, u.Host, u.Fragment = strings.ToLower(u.Scheme), strings.ToLower(u.Host), ""
	return u.String()
}
