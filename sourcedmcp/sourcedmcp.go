// Package sourcedmcp exposes sourced.net to AI apps as MCP tools:
// sourced_search, sourced_fetch, sourced_verify, and sourced_resolve. The
// same tools run on a resolver (answers signed by it) or locally (a
// validating client that checks everything itself).
//
// Each result is text written for the model to quote and cite from. The
// full signed answer can be added as structured content
// (WithStructuredContent), for apps and checkers; it is off by default
// because models read it too, and it repeats every passage.
package sourcedmcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver"
	"github.com/sourcednet/resolver/rank"
	"github.com/sourcednet/resolver/verifier"
)

// Backend answers the three operations. *resolver.Resolver is one; Local is
// the other.
type Backend interface {
	FetchAnswer(ctx context.Context, req resolver.FetchRequest) (*resolver.FetchAnswer, error)
	ResolveAnswer(ctx context.Context, citation string) (*resolver.ResolveAnswer, error)
	// VerifyAnswer returns a *resolver.VerifyAnswer with a URL, or a
	// *resolver.LookupAnswer without one.
	VerifyAnswer(ctx context.Context, passage, pageURL string) (any, error)
	SearchAnswer(ctx context.Context, req resolver.SearchRequest) (*resolver.SearchAnswer, error)
}

// FetchInput is sourced_fetch's input.
type FetchInput struct {
	URL       string `json:"url" jsonschema:"the https URL of the page to read"`
	Query     string `json:"query,omitempty" jsonschema:"optional: what you are looking for; the most relevant passages come first"`
	MaxChunks int    `json:"max_chunks,omitempty" jsonschema:"optional: return at most this many passages (default 5 with a query, all without)"`
	Offset    int    `json:"offset,omitempty" jsonschema:"optional: skip this many passages, to get the next page of results (a result says which offset to use)"`
	Around    string `json:"around,omitempty" jsonschema:"optional: the cite link of a passage on this page; returns it with the passages around it, in page order, instead of a ranking"`
	Context   int    `json:"context,omitempty" jsonschema:"optional, with around: how many passages on each side (default 1, at most 5)"`
}

// defaultQueryChunks is how many passages a query returns unless told
// otherwise: enough to answer, few enough to keep the model's context small.
const defaultQueryChunks = 5

// request is the fetch to make: all passages without a query, the default
// number with one, or what was asked for.
func (in FetchInput) request() resolver.FetchRequest {
	req := resolver.FetchRequest{URL: in.URL, Query: in.Query, MaxChunks: in.MaxChunks, Offset: in.Offset, Around: in.Around, Context: in.Context}
	if req.MaxChunks == 0 && in.Around == "" && (in.Query != "" || in.Offset > 0) {
		req.MaxChunks = defaultQueryChunks
	}
	return req
}

// ResolveInput is sourced_resolve's input.
type ResolveInput struct {
	Citation string `json:"citation" jsonschema:"a cite link returned by sourced_fetch or sourced_verify"`
}

// SearchInput is sourced_search's input.
type SearchInput struct {
	Query      string `json:"query" jsonschema:"what you are looking for, in plain words"`
	Publisher  string `json:"publisher,omitempty" jsonschema:"optional: only this publisher's pages, e.g. wikipedia.test"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"optional: at most this many pages (default 5)"`
	Offset     int    `json:"offset,omitempty" jsonschema:"optional: skip this many pages, to get the next page of results (a result says which offset to use)"`
}

// VerifyInput is sourced_verify's input.
type VerifyInput struct {
	Passage string `json:"passage" jsonschema:"the text to check, quoted as exactly as possible"`
	URL     string `json:"url,omitempty" jsonschema:"optional: the page the passage should be in; without it, all sources the resolver knows are searched"`
}

const (
	fetchDescription = `Read a web page as passages signed by its publisher (sourced.net), instead of scraping it.

Use it whenever you will quote or cite a page. Each passage comes with a "cite" link that points to that exact passage. When you use a passage:
- quote it exactly, and give its cite link;
- say "signed by <publisher>", never just "verified": a signature proves who published it, not that it is true;
- if the page's state is corrected or retracted, say so.

If the result says the site is not participating, it doesn't publish signed content: use your normal web fetch instead and don't call it signed. If it says the URL is unlisted, the publisher signs other pages: the result lists them, so fetch the right one.

Pass "query" to get the most relevant passages first, 5 at a time. If they don't answer the question and the result says more passages match, call again with the same query and the "offset" it gives, before concluding the page doesn't say.

To read a passage in context (what comes before and after it, say before quoting a claim), pass its cite link as "around".`

	searchDescription = `Find signed pages about a topic among the sources this resolver knows, when you don't have a URL. Returns pages, best first, each with its most relevant passage and a cite link; the first page comes with a few more of its passages.

It searches what the resolver holds, not the whole web: if none of the results fits and the result says more pages match, call again with the "offset" it gives; otherwise use your normal web search. To read more of a result, call sourced_fetch on its URL with your question as the query. Quote passages exactly and give their cite links, as with sourced_fetch.`

	resolveDescription = `Check what has happened to a cited passage since it was published: current, revised, corrected, retracted, or withdrawn. It always asks the publisher.

Use it before relying on a citation from earlier in the conversation or from memory, and whenever the user asks whether a source still stands. If the source was corrected or retracted, tell the user, with the date and the publisher's note, and prefer the latest version of the passage.`

	verifyDescription = `Check whether a passage was really published, and by whom.

With "url": whether the passage appears in that page's signed content. Without "url": which signed sources the resolver knows contain it, earliest first (dates are the publishers' claims). A fragment of a sentence is fine, but give at least 6 words. Matching is word for word and ignores case, punctuation, and whitespace; "partial" means only part of the passage matched, as with an edited quote.`
)

// Option configures NewServer.
type Option func(*options)

type options struct {
	log        *slog.Logger
	structured bool
}

// WithLogger logs every tool call: its arguments, outcome, and duration.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithStructuredContent adds the full signed answer to every result as
// structured content. It more than doubles what a model reads, so use it
// only for clients that check answers rather than read them; the HTTP API
// gives the same answers.
func WithStructuredContent() Option { return func(o *options) { o.structured = true } }

// NewServer returns an MCP server offering the three tools on b.
func NewServer(b Backend, name, version string, opts ...Option) *mcp.Server {
	o := options{log: slog.New(slog.DiscardHandler)}
	for _, opt := range opts {
		opt(&o)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "sourced_fetch", Description: fetchDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in FetchInput) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			a, err := b.FetchAnswer(ctx, in.request())
			if err != nil {
				o.log.Warn("tool", "name", "sourced_fetch", "url", in.URL, "err", err, "took", since(start))
				return nil, nil, err
			}
			o.log.Info("tool", "name", "sourced_fetch", "url", in.URL, "query", in.Query, "max_chunks", in.MaxChunks, "offset", in.Offset,
				"verification", string(a.Verification), "state", string(a.State), "chunks", len(a.Chunks), "next_offset", a.NextOffset, "took", since(start))
			return o.result(renderFetch(a, in.Around != ""), a), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "sourced_search", Description: searchDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			a, err := b.SearchAnswer(ctx, resolver.SearchRequest{Query: in.Query, Publisher: in.Publisher, MaxResults: in.MaxResults, Offset: in.Offset})
			if err != nil {
				o.log.Warn("tool", "name", "sourced_search", "query", in.Query, "err", err, "took", since(start))
				return nil, nil, err
			}
			o.log.Info("tool", "name", "sourced_search", "query", in.Query, "publisher", in.Publisher, "offset", in.Offset,
				"results", len(a.Results), "next_offset", a.NextOffset, "took", since(start))
			return o.result(renderSearch(a), a), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "sourced_resolve", Description: resolveDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ResolveInput) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			a, err := b.ResolveAnswer(ctx, in.Citation)
			if err != nil {
				o.log.Warn("tool", "name", "sourced_resolve", "citation", in.Citation, "err", err, "took", since(start))
				return nil, nil, err
			}
			o.log.Info("tool", "name", "sourced_resolve", "url", a.URL, "verification", string(a.Verification),
				"state", string(a.State), "unchanged", a.Unchanged, "took", since(start))
			return o.result(renderResolve(a), a), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "sourced_verify", Description: verifyDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in VerifyInput) (*mcp.CallToolResult, any, error) {
			start := time.Now()
			a, err := b.VerifyAnswer(ctx, in.Passage, in.URL)
			if err != nil {
				o.log.Warn("tool", "name", "sourced_verify", "url", in.URL, "passage", clip(in.Passage), "err", err, "took", since(start))
				return nil, nil, err
			}
			attrs := []any{"name", "sourced_verify", "url", in.URL, "passage", clip(in.Passage)}
			switch v := a.(type) {
			case *resolver.VerifyAnswer:
				attrs = append(attrs, "verification", string(v.Verification), "found", v.Found)
			case *resolver.LookupAnswer:
				attrs = append(attrs, "found", v.Found, "matches", len(v.Matches))
			}
			o.log.Info("tool", append(attrs, "took", since(start))...)
			return o.result(renderVerify(a), a), nil, nil
		})
	return s
}

func since(t time.Time) time.Duration { return time.Since(t).Round(time.Millisecond) }

// clip shortens a passage for a log line.
func clip(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

func (o options) result(text string, answer any) *mcp.CallToolResult {
	r := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
	if o.structured {
		r.StructuredContent = answer
	}
	return r
}

// --- Local backend -------------------------------------------------------------

// Local is a validating client: it fetches from publishers directly and
// checks every signature itself. Its answers say verified_by "local".
type Local struct {
	V   *verifier.Verifier
	Now func() time.Time
	// Ranker orders passages for queries; the zero value is rank.Default.
	Ranker rank.Ranker
}

func (l *Local) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// FetchAnswer implements Backend.
func (l *Local) FetchAnswer(ctx context.Context, req resolver.FetchRequest) (*resolver.FetchAnswer, error) {
	if req.MaxChunks < 0 || req.Offset < 0 || req.Context < 0 || req.Context > resolver.MaxContext {
		return nil, fmt.Errorf("max_chunks and offset: want non-negative numbers; context: 0 to %d", resolver.MaxContext)
	}
	p, err := l.V.Fetch(ctx, req.URL)
	if err != nil {
		return nil, err
	}
	a := &resolver.FetchAnswer{Page: *p}
	if req.Around != "" && p.Verification == verifier.Verified {
		if a.Chunks, err = resolver.Surroundings(p.Chunks, req.Around, req.Context); err != nil {
			return nil, err
		}
	} else {
		a.Chunks, a.Offset, a.NextOffset = resolver.SelectChunks(l.Ranker, p.Chunks, req)
	}
	a.VerifiedBy, a.AsOf = "local", l.now().UTC()
	return a, nil
}

// ResolveAnswer implements Backend.
func (l *Local) ResolveAnswer(ctx context.Context, citation string) (*resolver.ResolveAnswer, error) {
	res, err := l.V.Resolve(ctx, citation)
	if err != nil {
		return nil, err
	}
	a := &resolver.ResolveAnswer{Resolution: *res}
	a.VerifiedBy, a.AsOf = "local", l.now().UTC()
	return a, nil
}

// SearchAnswer implements Backend. A local client holds no index to search,
// so search needs a resolver.
func (l *Local) SearchAnswer(context.Context, resolver.SearchRequest) (*resolver.SearchAnswer, error) {
	return nil, errors.New("search needs a resolver; with only a local client, find the page's URL first and use sourced_fetch")
}

// VerifyAnswer implements Backend. Without a URL there is no index to
// search, so it needs a resolver.
func (l *Local) VerifyAnswer(ctx context.Context, passage, pageURL string) (any, error) {
	if pageURL == "" {
		return nil, errors.New("checking a passage without a URL needs a resolver; pass the page's url")
	}
	m, err := l.V.VerifyPassage(ctx, pageURL, passage)
	if err != nil {
		return nil, err
	}
	a := &resolver.VerifyAnswer{PassageMatch: *m}
	a.VerifiedBy, a.AsOf = "local", l.now().UTC()
	return a, nil
}

// --- Rendering for the model -----------------------------------------------------

func checkedBy(a resolver.Answer) string {
	if a.VerifiedBy == "local" {
		return "checked locally"
	}
	return fmt.Sprintf("checked by resolver %s, as of %s", a.VerifiedBy, a.AsOf.Format(time.RFC3339))
}

func quote(b *strings.Builder, c verifier.Chunk) {
	if len(c.Section) > 0 {
		fmt.Fprintf(b, "Section: %s\n", strings.Join(c.Section, " › "))
	}
	fmt.Fprintf(b, "Cite: %s\n<passage>\n%s\n</passage>\n", c.Cite, c.Text)
}

func notSigned(v verifier.Verification, reason core.Reason, detail string) string {
	switch v {
	case verifier.NotParticipating:
		return "This site does not publish signed content (not participating in sourced.net). Use a normal web fetch, and don't present what you read as signed."
	case verifier.Unlisted:
		return "The publisher signs content, but this URL is not one of its signed pages. Use sourced_fetch on the site to see which pages are signed."
	case verifier.Failed:
		return fmt.Sprintf("Verification FAILED (%s): %s\nDo not present this content as signed by the publisher.", reason, detail)
	}
	return ""
}

// renderFetch writes a fetch answer for the model. around says the
// passages are one passage and its surroundings, not a ranking.
func renderFetch(a *resolver.FetchAnswer, around bool) string {
	if a.Verification == verifier.Unlisted {
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n%s publishes signed content, but this URL is not one of its signed pages.\n", a.URL, a.Publisher)
		if len(a.Listed) > 0 {
			b.WriteString("Its signed pages include:\n")
			for _, u := range a.Listed {
				fmt.Fprintf(&b, "- %s\n", u)
			}
			b.WriteString("Fetch the one that matches what you need.\n")
		}
		return b.String()
	}
	if s := notSigned(a.Verification, a.Reason, a.Detail); s != "" {
		return a.URL + "\n" + s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nURL: %s\nSigned by %s · published %s · state: %s · %s\n",
		a.Title, a.URL, a.Publisher, a.PublishedAt.Format("2006-01-02"), a.State, checkedBy(a.Answer))
	if a.State == core.StateWithdrawn {
		b.WriteString("\nThe publisher has withdrawn this page. Its text is no longer available and must not be quoted.\n")
		return b.String()
	}
	switch {
	case len(a.Chunks) == 0 && a.Offset > 0:
		fmt.Fprintf(&b, "\nNo more passages after the first %d.\n", a.Offset)
		return b.String()
	case around:
		fmt.Fprintf(&b, "\n%d passages: the one asked about and those around it, in page order.", len(a.Chunks))
	case a.Offset > 0 || a.NextOffset > 0:
		fmt.Fprintf(&b, "\nPassages %d–%d, most relevant first.", a.Offset+1, a.Offset+len(a.Chunks))
	default:
		fmt.Fprintf(&b, "\n%d passages.", len(a.Chunks))
	}
	b.WriteString(" Quote them exactly and give the cite link of each passage you use.\n")
	for i, c := range a.Chunks {
		fmt.Fprintf(&b, "\n[%d] ", a.Offset+i+1)
		quote(&b, c)
	}
	if a.NextOffset > 0 {
		fmt.Fprintf(&b, "\nMore passages match. If these don't answer the question, call sourced_fetch again with the same query and offset %d.\n", a.NextOffset)
	}
	return b.String()
}

func renderSearch(a *resolver.SearchAnswer) string {
	var b strings.Builder
	switch {
	case len(a.Results) == 0 && a.Offset > 0:
		fmt.Fprintf(&b, "No more signed pages match %q after the first %d (%s). Try your normal web search.\n", a.Query, a.Offset, checkedBy(a.Answer))
		return b.String()
	case len(a.Results) == 0:
		fmt.Fprintf(&b, "No signed page this resolver knows matches %q (%s). Try your normal web search.\n", a.Query, checkedBy(a.Answer))
		return b.String()
	case a.Offset > 0 || a.NextOffset > 0:
		fmt.Fprintf(&b, "Signed pages %d–%d matching %q (%s). Best first; each with its most relevant passage.\n", a.Offset+1, a.Offset+len(a.Results), a.Query, checkedBy(a.Answer))
	default:
		fmt.Fprintf(&b, "%d signed pages match %q (%s). Best first; each with its most relevant passage.\n", len(a.Results), a.Query, checkedBy(a.Answer))
	}
	for i, r := range a.Results {
		fmt.Fprintf(&b, "\n[%d] %s\nURL: %s\nSigned by %s\n", a.Offset+i+1, r.Title, r.URL, r.Publisher)
		quote(&b, r.Passage)
		if len(r.MorePassages) > 0 {
			b.WriteString("More from this page:\n")
			for _, c := range r.MorePassages {
				quote(&b, c)
			}
		}
	}
	b.WriteString("\nFor more of a page, call sourced_fetch on its URL with your question as the query.\n")
	if a.NextOffset > 0 {
		fmt.Fprintf(&b, "More pages match: if none of these fits, call sourced_search again with offset %d.\n", a.NextOffset)
	}
	return b.String()
}

func renderResolve(a *resolver.ResolveAnswer) string {
	if s := notSigned(a.Verification, a.Reason, a.Detail); s != "" {
		return a.Citation + "\n" + s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nURL: %s\nSigned by %s · %s\n", a.Title, a.URL, a.Publisher, checkedBy(a.Answer))
	switch {
	case a.State == "":
		fmt.Fprintf(&b, "State: unknown. %s\n", a.Detail)
	case a.State == core.StateCurrent:
		b.WriteString("State: current. The cited passage is still what the publisher says.\n")
	default:
		fmt.Fprintf(&b, "State: %s.\n", strings.ToUpper(string(a.State)))
		for _, c := range a.Changes {
			fmt.Fprintf(&b, "- %s on %s", c.Change, c.PublishedAt.Format("2006-01-02"))
			if c.Note != "" {
				fmt.Fprintf(&b, ": %q", c.Note)
			}
			b.WriteString("\n")
		}
	}
	switch {
	case a.State == core.StateWithdrawn:
		b.WriteString("\nThe page was withdrawn. Do not quote the cited passage anymore.\n")
	case a.Unchanged && a.State != core.StateCurrent:
		b.WriteString("\nThis passage itself did not change; other parts of the page did.\n")
	}
	if a.Passage != nil && a.State != core.StateWithdrawn {
		b.WriteString("\nCited passage:\n")
		quote(&b, *a.Passage)
	}
	if a.Latest != nil && !a.Unchanged {
		b.WriteString("\nLatest version of this passage:\n")
		quote(&b, *a.Latest)
	}
	return b.String()
}

func renderVerify(v any) string {
	var b strings.Builder
	switch a := v.(type) {
	case *resolver.VerifyAnswer:
		if s := notSigned(a.Verification, a.Reason, a.Detail); s != "" {
			return a.URL + "\n" + s
		}
		if !a.Found {
			fmt.Fprintf(&b, "NOT FOUND in the signed content of %s (signed by %s, %s).\n", a.URL, a.Publisher, checkedBy(a.Answer))
			return b.String()
		}
		fmt.Fprintf(&b, "FOUND in %s, signed by %s (%s).\n\n", a.URL, a.Publisher, checkedBy(a.Answer))
		quote(&b, *a.Chunk)
	case *resolver.LookupAnswer:
		if !a.Found {
			fmt.Fprintf(&b, "No known signed source contains this passage (%s).\n", checkedBy(a.Answer))
			if a.Note != "" {
				b.WriteString(a.Note + "\n")
			}
			return b.String()
		}
		fmt.Fprintf(&b, "%d matching passages (%s). Earliest claimed publication first.\n", len(a.Matches), checkedBy(a.Answer))
		for i, m := range a.Matches {
			fmt.Fprintf(&b, "\n[%d] %s match, %.0f%% word overlap\n", i+1, m.Match, m.Overlap*100)
			for _, o := range m.Occurrences {
				cur := ""
				if o.Current {
					cur = ", current version"
				}
				fmt.Fprintf(&b, "- signed by %s, %s (claims %s%s)\n  Cite: %s\n", o.Publisher, o.URL, o.PublishedAt.Format("2006-01-02"), cur, o.Cite)
			}
			fmt.Fprintf(&b, "<passage>\n%s\n</passage>\n", m.Text)
		}
	}
	return b.String()
}
