// Package resolver is a sourced.net resolver: it syncs publishers, keeps
// what it has verified, and answers fetch, resolve, and verify requests for
// many clients, with every answer signed.
//
// It is a cache in front of a verifier.Verifier. Records and bundles never
// change, so they are kept forever (until withdrawn); the only thing that
// goes stale is which record is current for a page, which is trusted for a
// freshness window and then rechecked with the publisher.
package resolver

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/passage"
	"github.com/sourcednet/resolver/rank"
	"github.com/sourcednet/resolver/verifier"
)

// Config configures a Resolver.
type Config struct {
	// Name is the resolver's domain. Its answers name it, and clients fetch
	// its keys from https://<Name>/.well-known/sourced/resolver.json.
	Name    string
	DataDir string
	// HTTP is the client used to reach publishers.
	HTTP *http.Client
	// Freshness, PollInterval, MinPoll, MaxPoll, and AnnounceEvery set up
	// the default sync policy (see Polling) when Sync is nil.
	Freshness, PollInterval, MinPoll, MaxPoll, AnnounceEvery time.Duration
	// Sync decides when to check which publisher; nil is NewPolling(cfg).
	Sync SyncPolicy
	// Store keeps what the resolver verifies; nil opens a Store (SQLite
	// and chunk files) in DataDir. The resolver closes it.
	Store ContentStore
	// Publishers, if set, are the only publishers the resolver serves:
	// requests about any other domain fail with ErrNotServed, and it never
	// contacts them. Empty serves any publisher.
	Publishers []string
	// Operator and RetentionPolicy are published in resolver.json.
	Operator, RetentionPolicy string
	// MCPPath, if set, is where the resolver's MCP endpoint is served, and
	// is advertised in resolver.json.
	MCPPath string
	// Ranker orders passages for queries and search; nil is rank.Default.
	Ranker rank.Ranker
	// Index decides what the passage index stores; nil is passage.Default.
	Index passage.Index
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Resolver syncs publishers and answers requests.
type Resolver struct {
	cfg    Config
	store  ContentStore
	policy SyncPolicy
	v      *verifier.Verifier
	keyID  string
	priv   ed25519.PrivateKey
	log    *slog.Logger

	mu      sync.Mutex
	syncing map[string]*sync.Mutex
	wg      sync.WaitGroup

	// The indexer adds newly stored text to the passage index after the
	// request that brought it in has been answered.
	indexKick chan struct{}
	stopIndex context.CancelFunc
	indexDone chan struct{}
}

// New starts a resolver with cfg's parts, opening its store in cfg.DataDir
// unless cfg.Store is set, and creating its signing key there on first run.
func New(cfg Config) (*Resolver, error) {
	if cfg.Name == "" || strings.ContainsAny(cfg.Name, "/:") {
		return nil, fmt.Errorf("resolver name %q: want a bare domain", cfg.Name)
	}
	if cfg.DataDir == "" {
		return nil, errors.New("missing data directory")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Sync == nil {
		cfg.Sync = NewPolling(cfg)
	}
	store := cfg.Store
	if store == nil {
		s, err := OpenStore(cfg.DataDir, cfg.Index)
		if err != nil {
			return nil, err
		}
		store = s
	}
	keyID, priv, err := loadOrCreateKey(cfg.DataDir, cfg.Now())
	if err != nil {
		store.Close()
		return nil, err
	}
	r := &Resolver{
		cfg:       cfg,
		store:     store,
		policy:    cfg.Sync,
		keyID:     keyID,
		priv:      priv,
		log:       cfg.Logger,
		syncing:   map[string]*sync.Mutex{},
		indexKick: make(chan struct{}, 1),
		indexDone: make(chan struct{}),
	}
	r.v = verifier.New(cfg.HTTP, verifier.WithObserver(storeHooks{r}), verifier.WithCache(storeHooks{r}), verifier.WithClock(cfg.Now), verifier.WithLogger(cfg.Logger))
	ictx, stop := context.WithCancel(context.Background())
	r.stopIndex = stop
	go r.indexLoop(ictx)
	r.kickIndex() // chunks left queued by an earlier run or an upgrade
	return r, nil
}

// Close waits for background syncs, stops the indexer, and closes the
// store. Chunks still queued are indexed on the next start.
func (r *Resolver) Close() error {
	r.wg.Wait()
	r.stopIndex()
	<-r.indexDone
	return r.store.Close()
}

// kickIndex tells the indexer there may be new text to index.
func (r *Resolver) kickIndex() {
	select {
	case r.indexKick <- struct{}{}:
	default: // already told
	}
}

// indexDelay is how long the indexer waits after being kicked before it
// starts. Requests often come in bursts (a search, then several fetches,
// sometimes in parallel), and those are answered first. Lookups index what
// is still queued themselves, so the delay never makes them miss anything.
const indexDelay = 100 * time.Millisecond

// indexLoop indexes queued chunks whenever kicked, until ctx ends.
func (r *Resolver) indexLoop(ctx context.Context) {
	defer close(r.indexDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.indexKick:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(indexDelay):
		}
		start := time.Now()
		n, err := r.store.IndexPending(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			r.log.Error("index", "chunks", n, "err", err)
		case n > 0:
			r.log.Info("index", "chunks", n, "took", time.Since(start).Round(time.Millisecond))
		}
	}
}

// Store returns the resolver's store.
func (r *Resolver) Store() ContentStore { return r.store }

// Info returns the resolver's resolver.json content.
func (r *Resolver) Info() core.ResolverInfo {
	mcpURL := ""
	if r.cfg.MCPPath != "" {
		mcpURL = "https://" + r.cfg.Name + r.cfg.MCPPath
	}
	return core.ResolverInfo{
		MCP:             mcpURL,
		Spec:            core.SpecVersion,
		Resolver:        r.cfg.Name,
		Operator:        r.cfg.Operator,
		API:             "https://" + r.cfg.Name + "/sourced/v1/",
		Keys:            []core.Key{core.NewKey(r.keyID, r.priv.Public().(ed25519.PublicKey))},
		RetentionPolicy: r.cfg.RetentionPolicy,
	}
}

// storeHooks lets the verifier keep what it verifies in the store and read
// immutable objects back from it.
type storeHooks struct{ r *Resolver }

func (h storeHooks) ObserveKeys(pub string, raw []byte, at time.Time) {
	if err := h.r.store.PutKeys(context.Background(), pub, raw, at); err != nil {
		h.r.log.Error("store keys", "publisher", pub, "err", err)
	}
}

func (h storeHooks) CachedKeys(pub string) ([]byte, time.Time, bool) {
	return h.r.store.Keys(context.Background(), pub)
}

func (h storeHooks) ObserveRecord(raw []byte, rec *core.Record) {
	if err := h.r.store.PutRecord(context.Background(), raw, rec); err != nil {
		h.r.log.Error("store record", "record", rec.ID, "err", err)
	}
}

func (h storeHooks) ObserveBundle(rec *core.Record, b *core.Bundle) {
	if err := h.r.store.PutBundle(context.Background(), rec, b); err != nil {
		h.r.log.Error("store bundle", "record", rec.ID, "err", err)
	}
	h.r.kickIndex()
}

func (h storeHooks) CachedRecord(id string) ([]byte, bool) {
	raw, _, err := h.r.store.Record(context.Background(), id)
	return raw, err == nil && raw != nil
}

func (h storeHooks) CachedBundle(id string) (*core.Bundle, bool) {
	_, rec, err := h.r.store.Record(context.Background(), id)
	if err != nil || rec == nil {
		return nil, false
	}
	return h.r.store.Bundle(context.Background(), rec)
}

// --- Syncing -----------------------------------------------------------------

// AddPublisher starts tracking a publisher; it is synced on the next poll.
func (r *Resolver) AddPublisher(ctx context.Context, domain string) error {
	if err := r.serves(domain); err != nil {
		return err
	}
	_, err := r.store.AddPublisher(ctx, domain)
	return err
}

// ErrNotServed means a request is about a publisher this resolver doesn't
// serve (Config.Publishers).
var ErrNotServed = errors.New("this resolver doesn't serve that publisher")

// serves reports whether the resolver serves a publisher's domain.
func (r *Resolver) serves(domain string) error {
	if len(r.cfg.Publishers) == 0 || slices.ContainsFunc(r.cfg.Publishers, func(p string) bool { return strings.EqualFold(p, domain) }) {
		return nil
	}
	return fmt.Errorf("%w: %s (it serves %s)", ErrNotServed, domain, strings.Join(r.cfg.Publishers, ", "))
}

func (r *Resolver) syncLock(domain string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.syncing[domain]
	if !ok {
		m = &sync.Mutex{}
		r.syncing[domain] = m
	}
	return m
}

// Sync brings the store up to date with one publisher's manifest: new and
// changed pages are fetched and verified (only what isn't stored yet), the
// change feed records what changed, and withdrawals purge text.
func (r *Resolver) Sync(ctx context.Context, domain string) error {
	domain = strings.ToLower(domain)
	if err := r.serves(domain); err != nil {
		return err
	}
	lock := r.syncLock(domain)
	lock.Lock()
	defer lock.Unlock()

	if _, err := r.store.AddPublisher(ctx, domain); err != nil {
		return err
	}
	st, err := r.store.Publisher(ctx, domain)
	if err != nil {
		return err
	}
	now := r.cfg.Now()
	st.CheckedAt = now
	start := time.Now()

	res, err := r.v.Manifest(ctx, domain, st.ETag)
	switch {
	case errors.Is(err, verifier.ErrNotModified):
		r.log.Info("sync", "publisher", domain, "result", "not modified", "took", time.Since(start).Round(time.Millisecond))
		st.SyncedAt, st.NextAt, st.LastError = now, r.policy.NextSync(domain, now, 0, false), ""
		urls, err := r.store.CurrentURLs(ctx, domain)
		if err != nil {
			return err
		}
		for _, u := range urls {
			if id, _, ok, _ := r.store.Current(ctx, u); ok {
				r.store.SetCurrent(ctx, domain, u, id, now)
			}
		}
		return r.store.SavePublisher(ctx, st)
	case err != nil:
		st.LastError, st.NextAt = err.Error(), r.policy.NextSync(domain, now, 0, true)
		r.store.SavePublisher(ctx, st)
		r.log.Warn("sync failed", "publisher", domain, "err", err)
		return err
	}

	m := res.Manifest
	var errs []error
	changed := 0
	for _, u := range slices.Sorted(maps.Keys(m.Entries)) {
		c, err := r.point(ctx, domain, u, m.Entries[u], now)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u, err))
			r.log.Warn("sync page failed", "publisher", domain, "url", u, "err", err)
		}
		if c {
			changed++
		}
	}
	listed, err := r.store.CurrentURLs(ctx, domain)
	if err != nil {
		return err
	}
	for _, u := range listed {
		if _, ok := m.Entries[u]; !ok {
			r.store.ForgetCurrent(ctx, u)
		}
	}

	st.GeneratedAt, st.SyncedAt = m.GeneratedAt, now
	st.NextAt = r.policy.NextSync(domain, now, res.MaxAge, false)
	st.ETag, st.LastError = res.ETag, ""
	if len(errs) > 0 {
		st.ETag, st.LastError = "", errors.Join(errs...).Error() // retry everything next time
	}
	if err := r.store.SavePublisher(ctx, st); err != nil {
		return err
	}
	r.log.Info("sync", "publisher", domain, "result", "updated", "pages", len(m.Entries), "changed", changed,
		"errors", len(errs), "generated_at", m.GeneratedAt.Format(time.RFC3339), "took", time.Since(start).Round(time.Millisecond))
	return errors.Join(errs...)
}

// point makes url's current record id, fetching and verifying it (and its
// bundle and history) if needed. It records the change and purges text on
// withdrawal. at is when the publisher confirmed it. It reports whether the
// page's current record changed.
func (r *Resolver) point(ctx context.Context, pub, url, id string, at time.Time) (bool, error) {
	prev, _, had, err := r.store.Current(ctx, url)
	if err != nil {
		return false, err
	}
	rec, err := r.v.Record(ctx, pub, id)
	if err != nil {
		return false, err
	}
	if rec.Change != core.ChangeWithdrawal {
		if _, err := r.v.Bundle(ctx, rec); err != nil {
			return false, err
		}
	}
	// Keep the history's metadata, so resolves and purges can walk it.
	for sid, n := rec.Supersedes, 0; sid != "" && n < 1000; n++ {
		old, err := r.v.Record(ctx, pub, sid)
		if err != nil {
			break // history is best effort; resolve checks it live anyway
		}
		sid = old.Supersedes
	}
	if err := r.store.SetCurrent(ctx, pub, url, id, at); err != nil {
		return false, err
	}
	if had && prev == id {
		return false, nil
	}
	change := string(rec.Change)
	if change == "" {
		change = "new"
	}
	if err := r.store.AddChange(ctx, ChangeEvent{Publisher: pub, URL: url, Record: id, Change: change, SeenAt: at}); err != nil {
		return false, err
	}
	attrs := []any{"publisher", pub, "url", url, "change", change, "record", Short(id)}
	if rec.Note != "" {
		attrs = append(attrs, "note", rec.Note)
	}
	r.log.Info("change", attrs...)
	if rec.Change == core.ChangeWithdrawal {
		records, chunks, err := r.store.Purge(ctx, rec)
		if err != nil {
			return true, err
		}
		r.log.Info("purge", "url", url, "records", records, "chunks_deleted", chunks)
	}
	return true, nil
}

// Short shortens a record or chunk ID for logs: its first 12 hex digits.
func Short(id string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(id, core.RecordIDPrefix), core.ChunkIDPrefix)
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// Run polls due publishers until ctx ends.
func (r *Resolver) Run(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		due, err := r.store.DuePublishers(ctx, r.cfg.Now())
		if err != nil && ctx.Err() == nil {
			r.log.Error("due publishers", "err", err)
		}
		for _, d := range due {
			if err := r.Sync(ctx, d); err != nil && ctx.Err() == nil {
				r.log.Warn("sync", "publisher", d, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ErrRateLimited means a domain announced again too soon.
var ErrRateLimited = errors.New("announced too recently")

// Announce is a publisher's hint that it has published: the resolver syncs
// that publisher soon, in the background. The announce carries no content.
func (r *Resolver) Announce(domain string) error {
	domain = strings.ToLower(domain)
	if err := r.serves(domain); err != nil {
		return err
	}
	if !r.policy.AcceptAnnounce(domain, r.cfg.Now()) {
		return ErrRateLimited
	}
	r.log.Info("announce", "publisher", domain)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := r.Sync(ctx, domain); err != nil {
			r.log.Warn("announced sync", "publisher", domain, "err", err)
		}
	}()
	return nil
}

// WaitIdle waits for background syncs started by announces.
func (r *Resolver) WaitIdle() { r.wg.Wait() }

// --- Pages -------------------------------------------------------------------

// Page returns a page's verified content and when its current record was
// last confirmed with the publisher. Within the freshness window it answers
// from the store; after it, it asks the publisher. If the publisher can't be
// reached, it serves what it has, with the older time (serve-stale).
func (r *Resolver) Page(ctx context.Context, pageURL string) (*verifier.Page, time.Time, error) {
	if pub, err := hostOf(pageURL); err == nil {
		if err := r.serves(pub); err != nil {
			return nil, time.Time{}, err
		}
	}
	id, checked, ok, err := r.store.Current(ctx, pageURL)
	if err != nil {
		return nil, time.Time{}, err
	}
	now := r.cfg.Now()
	if ok && r.policy.Fresh(checked, now) {
		p, err := r.v.FetchRecord(ctx, pageURL, id)
		r.logPage(pageURL, "fresh", p, err)
		return p, checked, err
	}

	liveID, err := r.v.CurrentRecordID(ctx, pageURL)
	var nl *verifier.NotListedError
	switch {
	case errors.As(err, &nl):
		if ok {
			r.store.ForgetCurrent(ctx, pageURL)
		}
		p := &verifier.Page{URL: pageURL, Publisher: nl.Publisher}
		nl.Unlisted(p)
		r.logPage(pageURL, "live", p, nil)
		return p, now, nil
	case errors.Is(err, verifier.ErrNoRecord):
		if ok {
			r.store.ForgetCurrent(ctx, pageURL)
		}
		p := &verifier.Page{URL: pageURL, Verification: verifier.NotParticipating}
		r.logPage(pageURL, "live", p, nil)
		return p, now, nil
	case err != nil && core.ReasonOf(err) != "":
		p, err := r.failed(pageURL, err)
		r.logPage(pageURL, "live", p, err)
		return p, now, err
	case err != nil:
		if ok { // publisher unreachable: serve stale
			r.log.Warn("publisher unreachable, serving stale", "url", pageURL, "as_of", checked.Format(time.RFC3339), "err", err)
			p, ferr := r.v.FetchRecord(ctx, pageURL, id)
			r.logPage(pageURL, "stale", p, ferr)
			return p, checked, ferr
		}
		r.log.Warn("publisher unreachable", "url", pageURL, "err", err)
		return nil, time.Time{}, err
	}

	p, err := r.v.FetchRecord(ctx, pageURL, liveID)
	r.logPage(pageURL, "live", p, err)
	if err != nil {
		return nil, time.Time{}, err
	}
	if p.Verification == verifier.Verified {
		if _, err := r.store.AddPublisher(ctx, p.Publisher); err != nil {
			return nil, time.Time{}, err
		}
		if _, err := r.point(ctx, p.Publisher, pageURL, liveID, now); err != nil {
			return nil, time.Time{}, err
		}
	}
	return p, now, nil
}

// logPage logs how a page was answered: from the store within the
// freshness window, checked live with the publisher, or stale.
func (r *Resolver) logPage(pageURL, mode string, p *verifier.Page, err error) {
	if err != nil {
		r.log.Warn("page", "url", pageURL, "mode", mode, "err", err)
		return
	}
	if p == nil {
		return
	}
	attrs := []any{"url", pageURL, "mode", mode, "verification", string(p.Verification)}
	if p.Record != "" {
		attrs = append(attrs, "record", Short(p.Record), "state", string(p.State), "chunks", len(p.Chunks))
	}
	if p.Reason != "" {
		attrs = append(attrs, "reason", string(p.Reason))
	}
	r.log.Info("page", attrs...)
}

func (r *Resolver) failed(pageURL string, err error) (*verifier.Page, error) {
	p := &verifier.Page{URL: pageURL, Verification: verifier.Failed, Reason: core.ReasonOf(err), Detail: err.Error()}
	if pub, perr := hostOf(pageURL); perr == nil {
		p.Publisher = pub
	}
	return p, nil
}

// --- Keys ----------------------------------------------------------------------

type keyFile struct {
	ID   string `json:"id"`
	Seed string `json:"seed"`
}

// loadOrCreateKey returns the resolver's signing key, creating it on first run.
func loadOrCreateKey(dir string, now time.Time) (string, ed25519.PrivateKey, error) {
	path := filepath.Join(dir, "resolver-key.json")
	b, err := os.ReadFile(path)
	if err == nil {
		var k keyFile
		if err := json.Unmarshal(b, &k); err != nil {
			return "", nil, fmt.Errorf("%s: %w", path, err)
		}
		seed, err := base64.StdEncoding.DecodeString(k.Seed)
		if err != nil || len(seed) != ed25519.SeedSize {
			return "", nil, fmt.Errorf("%s: invalid key", path)
		}
		return k.ID, ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	_, priv, err := core.GenerateKey(nil)
	if err != nil {
		return "", nil, err
	}
	id := fmt.Sprintf("r%da", now.UTC().Year())
	b, err = json.Marshal(keyFile{ID: id, Seed: base64.StdEncoding.EncodeToString(priv.Seed())})
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", nil, err
	}
	return id, priv, nil
}
