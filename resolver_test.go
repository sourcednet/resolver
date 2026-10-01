package resolver_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/publisher"
	"github.com/sourcednet/publisher/check"
	"github.com/sourcednet/resolver"
	"github.com/sourcednet/resolver/verifier"
	"github.com/sourcednet/testkit/testnet"
	"github.com/sourcednet/testkit/testsite"
)

const (
	herald    = "daily-herald.test"
	library   = "example-library.test"
	devdocs   = "devdocs.test"
	bridgeURL = "https://daily-herald.test/news/2026/09/bridge-reopens.html"
	bridgeRel = "news/2026/09/bridge-reopens.html"
	digURL    = "https://example-library.test/guides/digitization.html"
)

var ctx = context.Background()

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	n     *testnet.Network
	r     *resolver.Resolver
	c     *resolver.Client
	clock *clock
}

func newEnv(t *testing.T, freshness time.Duration) *env {
	t.Helper()
	n := testnet.Standard(t)
	clk := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r, err := resolver.New(resolver.Config{
		Name: "resolver.test", DataDir: t.TempDir(), HTTP: n.Client(),
		Freshness: freshness, Now: clk.Now, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	n.AddHandler("resolver.test", r.Handler())
	return &env{n: n, r: r, c: &resolver.Client{Base: "https://resolver.test", HTTP: n.Client()}, clock: clk}
}

func (e *env) sync(t *testing.T, domains ...string) {
	t.Helper()
	for _, d := range domains {
		if err := e.r.Sync(ctx, d); err != nil {
			t.Fatalf("sync %s: %v", d, err)
		}
	}
}

func (e *env) fetch(t *testing.T, url string) *resolver.FetchAnswer {
	t.Helper()
	a, _, err := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func editPage(t *testing.T, s *testsite.Site, rel, old, new string) {
	t.Helper()
	page := s.Read(rel)
	if !strings.Contains(page, old) {
		t.Fatalf("%s does not contain %q", rel, old)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), filepath.FromSlash(rel)), []byte(strings.Replace(page, old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func textOf(a *resolver.FetchAnswer) string {
	var b strings.Builder
	for _, c := range a.Chunks {
		b.WriteString(c.Text + "\n")
	}
	return b.String()
}

func TestSignedAnswerChecksOut(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.sync(t, herald)
	a, raw, err := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: bridgeURL})
	if err != nil {
		t.Fatal(err)
	}
	if a.Verification != verifier.Verified || a.VerifiedBy != "resolver.test" || a.ResolverSig == nil || a.AsOf.IsZero() {
		t.Fatalf("answer: %+v", a.Answer)
	}
	if a.Originals != nil {
		t.Fatal("answer carries originals nobody asked for")
	}

	// The checker fetches the record from its publisher, or takes the
	// original the answer carries when asked for.
	f := check.HTTPFetcher{Client: e.n.Client()}
	if res := check.Answer(ctx, f, raw); !res.OK() || res.Records != 1 {
		t.Fatalf("check of a genuine answer: %+v", res.Findings)
	}
	withOriginals := *e.c
	withOriginals.Originals = true
	a2, raw2, err := withOriginals.FetchAnswer(ctx, resolver.FetchRequest{URL: bridgeURL})
	if err != nil || a2.Originals == nil || a2.Originals.Records[a2.Record] == nil {
		t.Fatalf("answer lacks the requested original: %v", err)
	}
	if res := check.Answer(ctx, f, raw2); !res.OK() || res.Records != 1 {
		t.Fatalf("check of an answer with originals: %+v", res.Findings)
	}

	// Changing what the publisher said is caught by the publisher's signature…
	quoted := strings.Replace(string(raw), "2.4 million", "9.9 million", 1)
	if res := check.Answer(ctx, f, []byte(quoted)); res.OK() {
		t.Fatal("altered passage passed the check")
	}
	// …and changing what the resolver said is caught by the resolver's.
	claimed := strings.Replace(string(raw), `"state": "current"`, `"state": "retracted"`, 1)
	res := check.Answer(ctx, f, []byte(claimed))
	if res.OK() || !strings.Contains(res.Findings[0].Message, "resolver signature") {
		t.Fatalf("altered claim: %+v", res.Findings)
	}
}

func TestResolverInfo(t *testing.T) {
	e := newEnv(t, time.Hour)
	resp, err := e.n.Client().Get("https://resolver.test/.well-known/sourced/resolver.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	info := e.r.Info()
	if info.Resolver != "resolver.test" || len(info.Keys) != 1 || info.API != "https://resolver.test/sourced/v1/" {
		t.Fatalf("info %+v", info)
	}
}

func TestQueryRanking(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.sync(t, herald)
	a, _, err := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: bridgeURL, Query: "are heavy trucks allowed on the bridge", MaxChunks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Chunks) != 1 || !strings.Contains(a.Chunks[0].Text, "Trucks over 7.5 tonnes") {
		t.Fatalf("top chunk: %+v", a.Chunks)
	}
}

func TestFetchPages(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.sync(t, herald)
	req := resolver.FetchRequest{URL: bridgeURL, Query: "bridge repairs", MaxChunks: 1}
	var seen []string
	for {
		a, _, err := e.c.FetchAnswer(ctx, req)
		if err != nil || len(a.Chunks) != 1 || a.Offset != req.Offset {
			t.Fatalf("offset %d: %+v, %v", req.Offset, a, err)
		}
		seen = append(seen, a.Chunks[0].ID)
		if a.NextOffset == 0 {
			break
		}
		req.Offset = a.NextOffset
	}
	all, _, _ := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: bridgeURL, Query: "bridge repairs"})
	if len(seen) != len(all.Chunks) || all.NextOffset != 0 {
		t.Fatalf("paged through %d passages, the whole ranking has %d", len(seen), len(all.Chunks))
	}
	for i, c := range all.Chunks {
		if seen[i] != c.ID {
			t.Fatalf("page %d holds %s, the ranking has %s there", i, seen[i], c.ID)
		}
	}
	var apiErr *resolver.APIError
	if _, _, err := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: bridgeURL, Offset: -1}); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("negative offset: %v", err)
	}
}

func TestFetchAround(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.sync(t, herald)
	all, _, err := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: bridgeURL})
	if err != nil || len(all.Chunks) < 3 {
		t.Fatalf("page: %+v, %v", all, err)
	}
	mid := all.Chunks[1]
	for _, req := range []resolver.FetchRequest{
		{URL: bridgeURL, Around: mid.Cite},
		{URL: bridgeURL, Around: mid.ID, Context: 1, Query: "ignored when around is set"},
	} {
		a, _, err := e.c.FetchAnswer(ctx, req)
		if err != nil || len(a.Chunks) != 3 || a.Chunks[0].ID != all.Chunks[0].ID || a.Chunks[1].ID != mid.ID || a.Chunks[2].ID != all.Chunks[2].ID {
			t.Fatalf("around %q: %+v, %v", req.Around, a, err)
		}
	}
	var apiErr *resolver.APIError
	if _, _, err := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: bridgeURL, Around: "sc:sha256:" + strings.Repeat("0", 64)}); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("passage not on the page: %v", err)
	}
}

func TestOnDemandFetchAddsPublisher(t *testing.T) {
	e := newEnv(t, time.Hour)
	a := e.fetch(t, "https://devdocs.test/docs/install.html") // never synced
	if a.Verification != verifier.Verified || len(a.Chunks) == 0 {
		t.Fatalf("on-demand fetch: %s %s", a.Reason, a.Detail)
	}
	ch, err := e.c.Changes(ctx, 0, devdocs)
	if err != nil || len(ch.Changes) != 1 || ch.Changes[0].Change != "new" {
		t.Fatalf("changes: %+v, %v", ch, err)
	}
	e.sync(t, devdocs) // the publisher is now tracked and syncs cleanly
}

func TestStaleFetchButLiveResolve(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.sync(t, herald)
	first := e.fetch(t, bridgeURL)
	var cite string
	for _, c := range first.Chunks {
		if strings.Contains(c.Text, "2.4 million") {
			cite = c.Cite
		}
	}

	site := e.n.Site(herald)
	editPage(t, site, bridgeRel, "cost 2.4 million", "cost 3.1 million")
	site.Build(testsite.T0.Add(time.Hour), map[string]publisher.Declaration{bridgeRel: {Change: core.ChangeCorrection, Note: "Wrong cost."}})
	e.clock.Add(10 * time.Minute)

	// Within the freshness window, fetch answers from the store, and says how old it is.
	stale := e.fetch(t, bridgeURL)
	if !strings.Contains(textOf(stale), "2.4 million") || !stale.AsOf.Equal(first.AsOf) {
		t.Fatalf("expected the cached version as of %s, got as of %s", first.AsOf, stale.AsOf)
	}
	// Resolve always checks the publisher live, so the correction shows at once.
	res, err := e.c.Resolve(ctx, cite)
	if err != nil || res.State != core.StateCorrected || res.Latest == nil || !strings.Contains(res.Latest.Text, "3.1 million") {
		t.Fatalf("live resolve: %+v, %v", res, err)
	}
	// After the next sync, fetch has it too, and the change feed records it.
	e.sync(t, herald)
	if fresh := e.fetch(t, bridgeURL); !strings.Contains(textOf(fresh), "3.1 million") {
		t.Fatal("fetch after sync still stale")
	}
	ch, _ := e.c.Changes(ctx, 0, herald)
	if last := ch.Changes[len(ch.Changes)-1]; last.Change != "correction" || last.URL != bridgeURL {
		t.Fatalf("last change %+v", last)
	}
}

func TestServeStaleWhenPublisherDown(t *testing.T) {
	e := newEnv(t, 0) // always recheck
	first := e.fetch(t, bridgeURL)
	e.clock.Add(time.Hour)
	e.n.Faults(herald).Down()

	a := e.fetch(t, bridgeURL)
	if a.Verification != verifier.Verified || !a.AsOf.Equal(first.AsOf) {
		t.Fatalf("serve-stale: %s, as of %s (first %s)", a.Verification, a.AsOf, first.AsOf)
	}
	if _, _, err := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: "https://daily-herald.test/news/2026/09/festival-dates.html"}); err == nil {
		t.Fatal("a page never fetched can't be served while its publisher is down")
	}
}

func TestSyncFetchesOnlyWhatChanged(t *testing.T) {
	e := newEnv(t, time.Hour)
	const bundles = "GET /.well-known/sourced/bundles/"
	const manifest = "GET /.well-known/sourced/manifest.json"
	e.sync(t, herald)
	if n := e.n.Requests(herald, bundles); n != 3 {
		t.Fatalf("first sync fetched %d bundles, want 3", n)
	}

	e.sync(t, herald) // nothing changed: the conditional request is answered 304
	st, _ := e.r.Store().Publisher(ctx, herald)
	if st.ETag == "" || st.LastError != "" || e.n.Requests(herald, bundles) != 3 || e.n.Requests(herald, manifest) != 2 {
		t.Fatalf("second sync: etag %q, error %q, %d bundles", st.ETag, st.LastError, e.n.Requests(herald, bundles))
	}

	site := e.n.Site(herald)
	editPage(t, site, bridgeRel, "on Monday morning", "on Monday at dawn")
	site.Build(testsite.T0.Add(time.Hour), nil)
	e.sync(t, herald)
	if n := e.n.Requests(herald, bundles); n != 4 {
		t.Fatalf("after one page changed: %d bundle requests, want 4", n)
	}
}

func TestWithdrawalPurgesStoredText(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.sync(t, library)
	a := e.fetch(t, digURL)
	chunk := a.Chunks[0]
	h := strings.TrimPrefix(chunk.ID, core.ChunkIDPrefix)
	get := func() int {
		resp, err := e.n.Client().Get("https://resolver.test/sourced/v1/objects/" + h)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get() != http.StatusOK {
		t.Fatal("chunk not served before withdrawal")
	}

	e.n.Site(library).Build(testsite.T0.Add(time.Hour), map[string]publisher.Declaration{digURL: {Change: core.ChangeWithdrawal}})
	e.sync(t, library)

	if get() != http.StatusNotFound {
		t.Fatal("withdrawn text is still served")
	}
	if w := e.fetch(t, digURL); w.State != core.StateWithdrawn || len(w.Chunks) != 0 {
		t.Fatalf("fetch after withdrawal: %+v", w.Page)
	}
	if l, err := e.c.Lookup(ctx, chunk.Text); err != nil || l.Found {
		t.Fatalf("withdrawn passage still found: %+v, %v", l, err)
	}
}

func TestPassageLookupWithoutURL(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.sync(t, herald, library, devdocs)

	l, err := e.c.Lookup(ctx, "The temporary ferry service that ran during the works stopped on Sunday evening.")
	if err != nil || !l.Found || l.Matches[0].Match != "exact" {
		t.Fatalf("exact quote: %+v, %v", l, err)
	}
	occ := l.Matches[0].Occurrences[0]
	if occ.Publisher != herald || occ.URL != bridgeURL || !occ.Current {
		t.Fatalf("occurrence %+v", occ)
	}
	if res, err := e.c.Resolve(ctx, occ.Cite); err != nil || res.State != core.StateCurrent {
		t.Fatalf("the returned citation should resolve: %+v, %v", res, err)
	}

	edited, _ := e.c.Lookup(ctx, "The temporary ferry service that ran during the works stopped on Saturday night. Trucks over 7.5 tonnes remain banned, as they were before the repairs, and the speed limit on the bridge stays at 30 km/h.")
	if !edited.Found || edited.Matches[0].Match != "partial" {
		t.Fatalf("lightly edited quote: %+v", edited)
	}
	if none, _ := e.c.Lookup(ctx, "Nothing like this sentence was ever published by anyone here."); none.Found {
		t.Fatal("unrelated passage found")
	}
}

func TestVerifyWithURL(t *testing.T) {
	e := newEnv(t, time.Hour)
	m, err := e.c.VerifyPassage(ctx, bridgeURL, "Trucks over 7.5 tonnes remain banned")
	if err != nil || !m.Found || m.Chunk == nil {
		t.Fatalf("verify: %+v, %v", m, err)
	}
}

func TestAnnounce(t *testing.T) {
	e := newEnv(t, time.Hour)
	if err := e.c.Announce(ctx, herald); err != nil {
		t.Fatal(err)
	}
	e.r.WaitIdle()
	if ch, _ := e.c.Changes(ctx, 0, herald); len(ch.Changes) != 3 {
		t.Fatalf("announce should sync the publisher: %+v", ch)
	}
	var apiErr *resolver.APIError
	if err := e.c.Announce(ctx, herald); !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("second announce: %v", err)
	}
	if err := e.c.Announce(ctx, "not a domain/"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("bad domain: %v", err)
	}
}

func TestChaosTamperedContentIsNotStored(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.n.Faults(herald).TamperBundles()
	if err := e.r.Sync(ctx, herald); err == nil {
		t.Fatal("sync of tampered bundles should report errors")
	}
	a := e.fetch(t, bridgeURL)
	if a.Verification != verifier.Failed || a.Reason != core.ReasonHashMismatch {
		t.Fatalf("got %s/%s", a.Verification, a.Reason)
	}
	e.n.Faults(herald).Reset()
	e.sync(t, herald)
	if a := e.fetch(t, bridgeURL); a.Verification != verifier.Verified {
		t.Fatalf("after the publisher is fixed: %s", a.Reason)
	}
}

func TestBadRequests(t *testing.T) {
	e := newEnv(t, time.Hour)
	var apiErr *resolver.APIError
	for _, u := range []string{"http://daily-herald.test/", "https://daily-herald.test:8443/", "not a url"} {
		if _, _, err := e.c.FetchAnswer(ctx, resolver.FetchRequest{URL: u}); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			t.Errorf("fetch %q: %v", u, err)
		}
	}
	if _, err := e.c.Lookup(ctx, "   "); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Errorf("empty passage: %v", err)
	}
}

func TestPublicOnlyRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	client := srv.Client()
	if err := resolver.PublicOnly(client); err != nil {
		t.Fatal(err)
	}
	_, err := client.Get(srv.URL)
	if !errors.Is(err, resolver.ErrPrivateAddress) {
		t.Fatalf("got %v, want ErrPrivateAddress", err)
	}
}

func TestStorePersistsAcrossRestarts(t *testing.T) {
	n := testnet.Standard(t)
	dir := t.TempDir()
	open := func() *resolver.Resolver {
		r, err := resolver.New(resolver.Config{Name: "resolver.test", DataDir: dir, HTTP: n.Client(), Freshness: time.Hour, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := open()
	key := r.Info().Keys[0]
	if err := r.Sync(ctx, herald); err != nil {
		t.Fatal(err)
	}
	r.Close()

	r = open()
	defer r.Close()
	if r.Info().Keys[0] != key {
		t.Fatal("resolver key changed across restarts")
	}
	n.Faults(herald).Down()
	p, _, err := r.Page(ctx, bridgeURL)
	if err != nil || p.Verification != verifier.Verified {
		t.Fatalf("stored page after restart: %+v, %v", p, err)
	}
}

func TestSearchWhatTheResolverHolds(t *testing.T) {
	e := newEnv(t, time.Hour)
	e.sync(t, herald, library, devdocs)

	a, err := e.c.Search(ctx, resolver.SearchRequest{Query: "trucks banned from the bridge", Publisher: "", MaxResults: 3})
	if err != nil || len(a.Results) == 0 || a.Results[0].URL != bridgeURL || !strings.Contains(a.Results[0].Passage.Text, "Trucks") {
		t.Fatalf("search: %+v, %v", a, err)
	}
	raw, _ := json.Marshal(a)
	if res := check.Answer(ctx, check.HTTPFetcher{Client: e.n.Client()}, raw); !res.OK() || res.Records == 0 {
		t.Fatalf("search answer should check out: %+v", res.Findings)
	}
	if a, _ := e.c.Search(ctx, resolver.SearchRequest{Query: "trucks banned from the bridge", Publisher: library, MaxResults: 3}); len(a.Results) != 0 {
		t.Fatalf("publisher filter ignored: %+v", a.Results)
	}
	if top := a.Results[0]; len(top.MorePassages) == 0 || len(top.MorePassages) >= resolver.TopResultPassages || top.MorePassages[0].ID == top.Passage.ID {
		t.Fatalf("first result's more passages: %+v", top.MorePassages)
	}
	for _, r := range a.Results[1:] {
		if len(r.MorePassages) != 0 {
			t.Fatalf("only the first result carries more passages: %+v", r)
		}
	}
	first, _ := e.c.Search(ctx, resolver.SearchRequest{Query: "bridge install guide", MaxResults: 1})
	second, err := e.c.Search(ctx, resolver.SearchRequest{Query: "bridge install guide", MaxResults: 1, Offset: first.NextOffset})
	if err != nil || first.NextOffset != 1 || len(second.Results) != 1 || second.Results[0].URL == first.Results[0].URL {
		t.Fatalf("next page of results: %+v then %+v, %v", first, second, err)
	}
	if _, err := e.c.Search(ctx, resolver.SearchRequest{Query: "  ", Publisher: "", MaxResults: 0}); err == nil {
		t.Fatal("empty query should be rejected")
	}
}

// countingStore is a content store that counts the records it is given.
type countingStore struct {
	*resolver.Store
	records int
}

func (s *countingStore) PutRecord(ctx context.Context, raw []byte, r *core.Record) error {
	s.records++
	return s.Store.PutRecord(ctx, raw, r)
}

// everyHour syncs hourly, never takes announces, and trusts what it holds.
type everyHour struct{}

func (everyHour) NextSync(_ string, now time.Time, _ time.Duration, _ bool) time.Time {
	return now.Add(time.Hour)
}
func (everyHour) AcceptAnnounce(string, time.Time) bool { return false }
func (everyHour) Fresh(time.Time, time.Time) bool       { return true }

func TestPluggableStoreAndSyncPolicy(t *testing.T) {
	n := testnet.Standard(t)
	dir := t.TempDir()
	sqlite, err := resolver.OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &countingStore{Store: sqlite}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	r, err := resolver.New(resolver.Config{
		Name: "resolver.test", DataDir: dir, HTTP: n.Client(), Store: store, Sync: everyHour{},
		Now: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Sync(ctx, herald); err != nil {
		t.Fatal(err)
	}
	if store.records == 0 {
		t.Fatal("the resolver didn't store records through the given store")
	}
	if st, _ := r.Store().Publisher(ctx, herald); !st.NextAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("next sync %v, want the policy's %v", st.NextAt, now.Add(time.Hour))
	}
	if err := r.Announce(herald); !errors.Is(err, resolver.ErrRateLimited) {
		t.Fatalf("announce the policy refuses: %v", err)
	}
}
