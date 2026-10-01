package verifier_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sourcednet/core"
	"github.com/sourcednet/publisher"
	"github.com/sourcednet/publisher/check"
	"github.com/sourcednet/resolver"
	"github.com/sourcednet/resolver/verifier"
	"github.com/sourcednet/testkit/testnet"
	"github.com/sourcednet/testkit/testsite"
)

const (
	library  = "example-library.test"
	herald   = "daily-herald.test"
	devdocs  = "devdocs.test"
	longform = "longform.test"
	plain    = "plain-site.test"

	bridgeURL = "https://daily-herald.test/news/2026/09/bridge-reopens.html"
	bridgeRel = "news/2026/09/bridge-reopens.html"
)

var ctx = context.Background()

func at(hours int) time.Time { return testsite.T0.Add(time.Duration(hours) * time.Hour) }

// api is what both a Verifier and a resolver Client offer, so every
// scenario can run directly and through a resolver.
type api interface {
	Fetch(ctx context.Context, pageURL string) (*verifier.Page, error)
	Resolve(ctx context.Context, citation string) (*verifier.Resolution, error)
	VerifyPassage(ctx context.Context, pageURL, passage string) (*verifier.PassageMatch, error)
}

// setup returns the standard network and a verifier that talks to it directly.
func setup(t *testing.T) (*testnet.Network, *verifier.Verifier) {
	t.Helper()
	n := testnet.Standard(t)
	return n, verifier.New(n.Client())
}

// viaResolver returns the standard network and a client for a resolver on
// it, at resolver.test, that rechecks publishers on every request.
func viaResolver(t *testing.T) (*testnet.Network, *resolver.Client) {
	t.Helper()
	n := testnet.Standard(t)
	r, err := resolver.New(resolver.Config{
		Name: "resolver.test", DataDir: t.TempDir(), HTTP: n.Client(),
		Freshness: 0, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	n.AddHandler("resolver.test", r.Handler())
	return n, &resolver.Client{Base: "https://resolver.test", HTTP: n.Client()}
}

// eachMode runs a scenario directly against a verifier, then through a resolver.
func eachMode(t *testing.T, scenario func(t *testing.T, n *testnet.Network, v api)) {
	t.Run("direct", func(t *testing.T) { n, v := setup(t); scenario(t, n, v) })
	t.Run("resolver", func(t *testing.T) { n, c := viaResolver(t); scenario(t, n, c) })
}

func mustFetch(t *testing.T, v api, url string) *verifier.Page {
	t.Helper()
	p, err := v.Fetch(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustResolve(t *testing.T, v api, cite string) *verifier.Resolution {
	t.Helper()
	r, err := v.Resolve(ctx, cite)
	if err != nil {
		t.Fatal(err)
	}
	if r.Verification != verifier.Verified {
		t.Fatalf("resolve %s: %s %s", cite, r.Reason, r.Detail)
	}
	return r
}

func wantFailed(t *testing.T, p *verifier.Page, reason core.Reason) {
	t.Helper()
	if p.Verification != verifier.Failed || p.Reason != reason {
		t.Fatalf("got %s/%s (%s), want failed/%s", p.Verification, p.Reason, p.Detail, reason)
	}
	if len(p.Chunks) != 0 {
		t.Fatal("a failed page must not carry content")
	}
}

// editPage rewrites a paragraph of a page in a site's web root.
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

// chunkWith returns the first chunk containing substr.
func chunkWith(t *testing.T, p *verifier.Page, substr string) verifier.Chunk {
	t.Helper()
	for _, c := range p.Chunks {
		if strings.Contains(c.Text, substr) {
			return c
		}
	}
	t.Fatalf("no chunk of %s contains %q", p.URL, substr)
	return verifier.Chunk{}
}

// --- The network itself --------------------------------------------------

func TestStandardSitesPassLiveCheck(t *testing.T) {
	n := testnet.Standard(t)
	f := check.HTTPFetcher{Client: n.Client()}
	for _, domain := range []string{library, herald, devdocs, longform} {
		res := check.Publisher(ctx, f, domain, check.Options{PagesRequired: true, LinkHeader: true})
		if !res.OK() || len(res.Findings) != 0 || res.Pages == 0 {
			t.Errorf("%s: %d pages, findings %+v", domain, res.Pages, res.Findings)
		}
	}
}

func TestUnknownDomainAndPlainHTTP(t *testing.T) {
	_, v := setup(t)
	_, err := v.Fetch(ctx, "https://nowhere.test/")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Fatalf("unknown domain: got %v, want a DNS error", err)
	}
	if _, err := v.Fetch(ctx, "http://daily-herald.test/"); err == nil {
		t.Fatal("plain HTTP must be refused")
	}
}

// --- Everyday publishing ---------------------------------------------------

func TestFetchVerifiedPages(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {

		p := mustFetch(t, v, bridgeURL)
		if p.Verification != verifier.Verified || p.State != core.StateCurrent || p.Publisher != herald {
			t.Fatalf("got %+v", p)
		}
		// Four paragraphs, each over the 150-character minimum, so four chunks.
		if p.Title != "Old stone bridge reopens after repairs" || len(p.Chunks) != 4 {
			t.Fatalf("title %q, %d chunks", p.Title, len(p.Chunks))
		}
		drivers := chunkWith(t, p, "Trucks over 7.5 tonnes")
		if strings.Join(drivers.Section, " / ") != "Old stone bridge reopens after repairs / What changes for drivers" {
			t.Fatalf("section %q", drivers.Section)
		}
		c, err := core.ParseCitation(drivers.Cite)
		if err != nil || c.Chunk != drivers.ID || c.Record != p.Record {
			t.Fatalf("citation %s: %+v, %v", drivers.Cite, c, err)
		}
		for _, chunk := range p.Chunks {
			if strings.Contains(chunk.Text, "Site header") || strings.Contains(chunk.Text, "Site footer") {
				t.Fatalf("site chrome leaked into content: %q", chunk.Text)
			}
		}

		// Technical docs: tables and code survive the conversion.
		cfg := mustFetch(t, v, "https://devdocs.test/docs/configuration.html")
		if !strings.Contains(chunkWith(t, cfg, "| interval").Text, "| Setting") {
			t.Fatal("table header lost")
		}
		if !strings.Contains(chunkWith(t, cfg, `algorithm = "blake3"`).Text, "```") {
			t.Fatal("code block lost its fence")
		}

		// Long form: an oversize section is split, never beyond the maximum.
		essay := mustFetch(t, v, "https://longform.test/essays/keeping-things.html")
		grew := 0
		for _, chunk := range essay.Chunks {
			if n := utf8.RuneCountInString(chunk.Text); n > core.DefaultChunkParams.Max {
				t.Fatalf("chunk of %d characters", n)
			}
			if len(chunk.Section) == 2 && chunk.Section[1] == "How it grew" {
				grew++
			}
		}
		if grew < 2 {
			t.Fatalf("oversize section became %d chunks, want at least 2", grew)
		}
		chunkWith(t, essay, "> We never had a policy")
	})
}

func TestNotParticipating(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		p := mustFetch(t, v, "https://plain-site.test/")
		if p.Verification != verifier.NotParticipating || len(p.Chunks) != 0 {
			t.Fatalf("got %+v", p)
		}
		// A participating publisher that doesn't list the URL: unlisted, with its signed pages.
		missing := mustFetch(t, v, "https://daily-herald.test/news/no-such-story.html")
		if missing.Verification != verifier.Unlisted || !slices.Contains(missing.Listed, bridgeURL) {
			t.Fatalf("unlisted page: got %s, listed %v", missing.Verification, missing.Listed)
		}
	})
}

func TestCorrectionLifecycle(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		site := n.Site(herald)
		p := mustFetch(t, v, bridgeURL)
		cost := chunkWith(t, p, "2.4 million")
		drivers := chunkWith(t, p, "Trucks over 7.5 tonnes")
		if r := mustResolve(t, v, cost.Cite); r.State != core.StateCurrent || !r.Unchanged {
			t.Fatalf("fresh citation: %+v", r)
		}

		editPage(t, site, bridgeRel, "cost 2.4 million, slightly under", "cost 3.1 million, somewhat over")
		note := "An earlier version gave the wrong cost of the repairs."
		site.Build(at(1), map[string]publisher.Declaration{bridgeRel: {Change: core.ChangeCorrection, Note: note}})

		r := mustResolve(t, v, cost.Cite)
		if r.State != core.StateCorrected || r.Unchanged {
			t.Fatalf("state %s, unchanged %v; want corrected and changed", r.State, r.Unchanged)
		}
		if len(r.Changes) != 1 || r.Changes[0].Note != note || r.Changes[0].Change != core.ChangeCorrection {
			t.Fatalf("changes %+v", r.Changes)
		}
		if r.Passage == nil || !strings.Contains(r.Passage.Text, "2.4 million") {
			t.Fatal("the original cited text should still be available")
		}
		if r.Latest == nil || !strings.Contains(r.Latest.Text, "3.1 million") {
			t.Fatalf("latest passage %+v", r.Latest)
		}

		// The page was corrected, but this passage didn't change.
		r = mustResolve(t, v, drivers.Cite)
		if r.State != core.StateCorrected || !r.Unchanged || r.Latest.ID != drivers.ID {
			t.Fatalf("untouched passage: state %s, unchanged %v", r.State, r.Unchanged)
		}

		if p := mustFetch(t, v, bridgeURL); !strings.Contains(chunkWith(t, p, "million").Text, "3.1 million") {
			t.Fatal("fetch should now return the corrected text")
		}
	})
}

func TestRevisionAndRetraction(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		site := n.Site(herald)
		const rel = "news/2026/09/festival-dates.html"
		cite := mustFetch(t, v, "https://daily-herald.test/"+rel).Chunks[0].Cite

		editPage(t, site, rel, "on Tuesday", "on Tuesday afternoon")
		site.Build(at(1), nil)
		if r := mustResolve(t, v, cite); r.State != core.StateRevised {
			t.Fatalf("after a revision: %s", r.State)
		}

		editPage(t, site, rel, "Entry remains free", "This story was retracted. Entry remains free")
		site.Build(at(2), map[string]publisher.Declaration{rel: {Change: core.ChangeRetraction, Note: "The festival was not moved."}})
		r := mustResolve(t, v, cite)
		if r.State != core.StateRetracted || len(r.Changes) != 2 || r.Changes[0].Note != "The festival was not moved." {
			t.Fatalf("after a retraction: %s, changes %+v", r.State, r.Changes)
		}
	})
}

func TestWithdrawalPurgesText(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		site := n.Site(library)
		const url = "https://example-library.test/guides/digitization.html"
		p := mustFetch(t, v, url)
		cite := p.Chunks[0].Cite

		site.Build(at(1), map[string]publisher.Declaration{url: {Change: core.ChangeWithdrawal, Note: "Removed on request."}})

		r := mustResolve(t, v, cite)
		if r.State != core.StateWithdrawn || r.Passage != nil || r.Latest != nil {
			t.Fatalf("withdrawn: state %s, passage %v, latest %v", r.State, r.Passage, r.Latest)
		}
		// The page file is still served, but without a link; the manifest says withdrawn.
		w := mustFetch(t, v, url)
		if w.Verification != verifier.Verified || w.State != core.StateWithdrawn || len(w.Chunks) != 0 {
			t.Fatalf("fetch after withdrawal: %+v", w)
		}
	})
}

func TestKeyRotationKeepsCitations(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		site := n.Site(library)
		const rel = "guides/book-care.html"
		const url = "https://example-library.test/" + rel
		cite := chunkWith(t, mustFetch(t, v, url), "relative humidity").Cite // keys now cached
		oldKey := site.Config.SigningKey

		if _, err := publisher.AddKey(site.Config, at(1)); err != nil {
			t.Fatal(err)
		}
		editPage(t, site, rel, "between 60 and 70 percent", "between 40 and 50 percent")
		site.Build(at(2), map[string]publisher.Declaration{rel: {Change: core.ChangeCorrection, Note: "Corrected the humidity range."}})

		// The new record is signed with a key the cached key set doesn't have.
		p := mustFetch(t, v, url)
		if p.Verification != verifier.Verified {
			t.Fatalf("after rotation: %s %s", p.Reason, p.Detail)
		}

		if _, err := publisher.RevokeKey(site.Config, oldKey); err != nil {
			t.Fatal(err)
		}
		r := mustResolve(t, v, cite)
		if r.State != core.StateCorrected || r.Passage == nil {
			t.Fatalf("old citation after revocation: %+v", r)
		}
	})
}

func TestVerifyPassage(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		m, err := v.VerifyPassage(ctx, bridgeURL, "Trucks over   7.5 tonnes\nremain banned")
		if err != nil || !m.Found || m.Chunk == nil {
			t.Fatalf("passage not found: %+v, %v", m, err)
		}
		m, err = v.VerifyPassage(ctx, bridgeURL, "Trucks over 3.5 tonnes remain banned")
		if err != nil || m.Found || m.Verification != verifier.Verified {
			t.Fatalf("altered passage: %+v, %v", m, err)
		}
		m, err = v.VerifyPassage(ctx, "https://plain-site.test/", "ordinary web pages")
		if err != nil || m.Found || m.Verification != verifier.NotParticipating {
			t.Fatalf("plain site: %+v, %v", m, err)
		}
	})
}

// --- Chaos -------------------------------------------------------------------

func TestChaosTamperedBundle(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		n.Faults(herald).TamperBundles()
		wantFailed(t, mustFetch(t, v, bridgeURL), core.ReasonHashMismatch)
	})
}

func TestChaosWrongKeys(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		n.Faults(herald).WrongKeys()
		wantFailed(t, mustFetch(t, v, bridgeURL), core.ReasonBadSignature)
	})
}

func TestChaosRevokedKeys(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		n.Faults(herald).RevokeKeys()
		wantFailed(t, mustFetch(t, v, bridgeURL), core.ReasonRevokedKey)
	})
}

func TestChaosMissingLinks(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		n.Faults(herald).StripLinks()
		// Verifiers fall back to the manifest, so content still verifies…
		if p := mustFetch(t, v, bridgeURL); p.Verification != verifier.Verified {
			t.Fatalf("manifest fallback: %s %s", p.Reason, p.Detail)
		}
		// …but the checker flags the missing links.
		res := check.Publisher(ctx, check.HTTPFetcher{Client: n.Client()}, herald, check.Options{PagesRequired: true})
		if res.OK() {
			t.Fatal("check should fail when pages lack their record link")
		}
	})
}

func TestChaosNoLinkHeader(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		n.Faults(herald).NoLinkHeader()
		if p := mustFetch(t, v, bridgeURL); p.Verification != verifier.Verified {
			t.Fatalf("<link> fallback: %s %s", p.Reason, p.Detail)
		}
	})
}

func TestChaosManifestRollback(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		site := n.Site(herald)
		f := n.Faults(herald)
		f.StripLinks() // force verifiers onto the manifest
		old := []byte(site.Read(".well-known/sourced/manifest.json"))
		mustFetch(t, v, bridgeURL)

		editPage(t, site, bridgeRel, "on Monday morning", "on Monday at dawn")
		site.Build(at(1), nil)
		if p := mustFetch(t, v, bridgeURL); p.Verification != verifier.Verified {
			t.Fatalf("newer manifest: %s", p.Reason)
		}

		f.ServeManifest(old)
		wantFailed(t, mustFetch(t, v, bridgeURL), core.ReasonStaleManifest)
		// A verifier that never saw the newer manifest can't tell; this is why
		// resolvers keep what they've seen.
		if p := mustFetch(t, verifier.New(n.Client()), bridgeURL); p.Verification != verifier.Verified {
			t.Fatalf("fresh verifier: %s", p.Reason)
		}
	})
}

func TestChaosForeignRecordLink(t *testing.T) {
	eachMode(t, func(t *testing.T, n *testnet.Network, v api) {
		lib := mustFetch(t, v, "https://example-library.test/guides/book-care.html")
		h, _ := core.IDHex(lib.Record, core.RecordIDPrefix)
		docs := n.Site(devdocs)

		// A page that links to another publisher's record.
		n.Faults(devdocs).NoLinkHeader()
		editPage(t, docs, "docs/install.html", `href="/.well-known/sourced/records/`, `href="https://example-library.test/.well-known/sourced/records/`)
		wantFailed(t, mustFetch(t, v, "https://devdocs.test/docs/install.html"), core.ReasonURLMismatch)

		// A publisher that serves another publisher's signed record as its own.
		cfg := mustFetch(t, v, "https://devdocs.test/docs/configuration.html")
		rec := n.Site(library).Read(".well-known/sourced/records/" + h + ".json")
		if err := os.WriteFile(docs.RecordPath(cfg.Record), []byte(rec), 0o644); err != nil {
			t.Fatal(err)
		}
		// A verifier without a cache downloads the swapped record and rejects it.
		// A resolver that already holds the genuine record keeps serving it:
		// records are immutable, so a cached copy that still verifies is correct.
		p := mustFetch(t, v, "https://devdocs.test/docs/configuration.html")
		genuine := p.Verification == verifier.Verified && p.Record == cfg.Record && p.Publisher == devdocs
		if !genuine && (p.Verification != verifier.Failed || p.Reason != core.ReasonURLMismatch) {
			t.Fatalf("got %s/%s for record %s; want the genuine record or url-mismatch", p.Verification, p.Reason, p.Record)
		}
	})
}

func TestChaosUnreachablePublisher(t *testing.T) {
	for name, fault := range map[string]func(*testnet.Faults){
		"server error":       func(f *testnet.Faults) { f.Status(500) },
		"connection dropped": func(f *testnet.Faults) { f.Down() },
		"too slow":           func(f *testnet.Faults) { f.Delay(2 * time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			n, v := setup(t)
			fault(n.Faults(herald))
			c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			defer cancel()
			if p, err := v.Fetch(c, bridgeURL); err == nil {
				t.Fatalf("want an error, got %s/%s", p.Verification, p.Reason)
			}
			n.Faults(herald).Reset()
			if p := mustFetch(t, v, bridgeURL); p.Verification != verifier.Verified {
				t.Fatalf("after reset: %s", p.Reason)
			}
		})
	}
}

// TestRedirectsStayOnTheHost: a publisher's files must come from its own
// domain over HTTPS, so a redirect elsewhere is refused before it is
// followed.
func TestRedirectsStayOnTheHost(t *testing.T) {
	n := testnet.Standard(t)
	n.AddHandler("moved.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://daily-herald.test"+r.URL.Path, http.StatusFound)
	}))
	before := n.Requests("daily-herald.test", "")
	_, err := verifier.New(n.Client()).Fetch(ctx, "https://moved.test/news/2026/09/bridge-reopens.html")
	if !errors.Is(err, verifier.ErrRedirect) {
		t.Fatalf("got %v, want ErrRedirect", err)
	}
	if n.Requests("daily-herald.test", "") != before {
		t.Fatal("the redirect was followed to the other host")
	}
}
