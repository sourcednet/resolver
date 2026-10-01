package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/rank"
	"github.com/sourcednet/resolver/verifier"
)

// APIPrefix is where the resolver's HTTP API lives.
const APIPrefix = "/sourced/v1/"

// Handler serves the HTTP API and resolver.json.
func (r *Resolver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+core.WellKnownPath+"resolver.json", r.handleInfo)
	mux.HandleFunc("GET "+APIPrefix+"fetch", r.handleFetch)
	mux.HandleFunc("GET "+APIPrefix+"resolve", r.handleResolve)
	mux.HandleFunc("POST "+APIPrefix+"verify", r.handleVerify)
	mux.HandleFunc("GET "+APIPrefix+"search", r.handleSearch)
	mux.HandleFunc("GET "+APIPrefix+"objects/{hex}", r.handleObject)
	mux.HandleFunc("GET "+APIPrefix+"changes", r.handleChanges)
	mux.HandleFunc("POST "+APIPrefix+"announce", r.handleAnnounce)
	return mux
}

// maxRequest caps request bodies.
const maxRequest = 64 << 10

func (r *Resolver) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, r.Info())
}

// ErrBadInput marks a request the resolver can't act on: a malformed URL,
// citation, or passage.
var ErrBadInput = errors.New("bad input")

func badInput(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadInput, fmt.Sprintf(format, args...))
}

// FetchRequest asks for a page's verified passages.
type FetchRequest struct {
	URL string
	// Query ranks the passages, most relevant first. Without it they keep
	// the page's order.
	Query string
	// MaxChunks is how many passages to return; 0 returns all.
	MaxChunks int
	// Offset skips that many passages first, for the next page of results.
	Offset int
	// Around, a cite link or chunk ID of a passage on the page, asks for
	// that passage and Context passages on each side, in page order,
	// instead of a ranking.
	Around string
	// Context is how many passages on each side Around adds: 1 if 0, at
	// most MaxContext.
	Context int
}

// MaxContext is the most passages on each side a fetch Around returns.
const MaxContext = 5

// FetchAnswer returns the signed answer to a fetch: the page's verified
// passages, as many and in the order the request asks for.
func (r *Resolver) FetchAnswer(ctx context.Context, req FetchRequest) (*FetchAnswer, error) {
	if _, err := hostOf(req.URL); err != nil {
		return nil, badInput("%v", err)
	}
	if req.MaxChunks < 0 || req.Offset < 0 || req.Context < 0 || req.Context > MaxContext {
		return nil, badInput("max_chunks and offset: want non-negative numbers; context: 0 to %d", MaxContext)
	}
	p, asOf, err := r.Page(ctx, req.URL)
	if err != nil {
		return nil, err
	}
	a := &FetchAnswer{Page: *p}
	if req.Around != "" && p.Verification == verifier.Verified {
		if a.Chunks, err = Surroundings(p.Chunks, req.Around, req.Context); err != nil {
			return nil, err
		}
	} else {
		a.Chunks, a.Offset, a.NextOffset = SelectChunks(r.cfg.Ranker, p.Chunks, req)
	}
	a.AsOf = asOf
	return a, r.sign(a)
}

// Surroundings returns the passage around names (a cite link or a chunk
// ID) and context passages on each side of it (1 if context is 0), in page
// order. It is bad input if the passage isn't in cs, for example because
// the page changed since it was cited.
func Surroundings(cs []verifier.Chunk, around string, context int) ([]verifier.Chunk, error) {
	id := around
	if c, err := core.ParseCitation(around); err == nil {
		id = c.Chunk
	}
	hex := strings.TrimPrefix(id, core.ChunkIDPrefix)
	if context == 0 {
		context = 1
	}
	for i, c := range cs {
		if strings.TrimPrefix(c.ID, core.ChunkIDPrefix) == hex {
			return cs[max(0, i-context):min(len(cs), i+context+1)], nil
		}
	}
	return nil, badInput("around: the passage is not in this page's current version (it may have changed: check it with resolve)")
}

// SelectChunks returns the passages a fetch asks for: all of them in page
// order, or one page of them ranked by the query. It also returns the
// request's offset and, if more passages follow, the next page's offset
// (otherwise 0).
func SelectChunks(rk rank.Ranker, cs []verifier.Chunk, req FetchRequest) (chunks []verifier.Chunk, offset, next int) {
	if req.Query == "" && req.MaxChunks <= 0 && req.Offset <= 0 {
		return cs, 0, 0
	}
	ps := make([]rank.Passage, len(cs))
	for i, c := range cs {
		ps[i] = rank.Passage{Section: c.Section, Text: c.Text}
	}
	page, more := rank.Slice(rank.Order(rk, req.Query, ps), req.Offset, req.MaxChunks)
	for _, i := range page {
		chunks = append(chunks, cs[i])
	}
	if more {
		next = req.Offset + len(page)
	}
	return chunks, req.Offset, next
}

// ResolveAnswer returns the signed answer to a resolve, checked live.
func (r *Resolver) ResolveAnswer(ctx context.Context, cite string) (*ResolveAnswer, error) {
	if cite == "" {
		return nil, badInput("missing citation")
	}
	res, err := r.v.Resolve(ctx, cite)
	if err != nil {
		return nil, err
	}
	cited := ""
	if c, err := core.ParseCitation(cite); err == nil {
		cited = Short(c.Record)
	}
	r.log.Info("resolve", "url", res.URL, "cited", cited, "current", Short(res.Current), "verification", string(res.Verification),
		"state", string(res.State), "unchanged", res.Unchanged, "changes", len(res.Changes))
	if res.State == core.StateWithdrawn && res.Current != "" {
		// Learned of a withdrawal before syncing it: purge now.
		if _, err := r.point(ctx, res.Publisher, res.URL, res.Current, r.cfg.Now()); err != nil {
			r.log.Error("purge on resolve", "url", res.URL, "err", err)
		}
	}
	a := &ResolveAnswer{Resolution: *res}
	a.AsOf = r.cfg.Now()
	return a, r.sign(a)
}

// VerifyAnswer returns the signed answer to a verify: a *VerifyAnswer when
// pageURL is set, otherwise a *LookupAnswer from the passage index.
func (r *Resolver) VerifyAnswer(ctx context.Context, passage, pageURL string) (any, error) {
	if strings.TrimSpace(passage) == "" {
		return nil, badInput("missing passage")
	}
	if pageURL == "" {
		hits, err := r.store.LookupPassage(ctx, passage, 10)
		if err != nil {
			return nil, err
		}
		a := &LookupAnswer{Passage: passage, Found: len(hits) > 0, Matches: hits}
		total, known, _ := r.store.PassageWindows(ctx, passage)
		best := ""
		if len(hits) > 0 {
			best = fmt.Sprintf("%s %.0f%%", hits[0].Match, hits[0].Overlap*100)
		}
		r.log.Info("lookup", "words", len(rank.Tokens(passage)), "windows", total, "windows_known", known, "matches", len(hits), "best", best)
		if min := r.store.MinPassageWords(); len(hits) == 0 && len(rank.Tokens(passage)) < min {
			a.Note = fmt.Sprintf("The passage may be too short to look up: give at least %d words.", min)
		}
		a.AsOf = r.cfg.Now()
		return a, r.sign(a)
	}
	if _, err := hostOf(pageURL); err != nil {
		return nil, badInput("%v", err)
	}
	p, asOf, err := r.Page(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	a := &VerifyAnswer{PassageMatch: *verifier.MatchPassage(p, passage)}
	a.AsOf = asOf
	return a, r.sign(a)
}

// counts reads non-negative whole numbers from query parameters; a missing
// one is 0.
func counts(q url.Values, names ...string) ([]int, error) {
	out := make([]int, len(names))
	for i, name := range names {
		if s := q.Get(name); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%s: want a non-negative number", name)
			}
			out[i] = n
		}
	}
	return out, nil
}

func (r *Resolver) handleFetch(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	n, err := counts(q, "max_chunks", "offset", "context")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a, err := r.FetchAnswer(req.Context(), FetchRequest{URL: q.Get("url"), Query: q.Get("query"), MaxChunks: n[0], Offset: n[1], Around: q.Get("around"), Context: n[2]})
	if err == nil && wantsOriginals(q.Get("originals")) {
		err = r.AttachOriginals(req.Context(), a)
	}
	writeResult(w, a, err)
}

func (r *Resolver) handleResolve(w http.ResponseWriter, req *http.Request) {
	a, err := r.ResolveAnswer(req.Context(), req.URL.Query().Get("cite"))
	if err == nil && wantsOriginals(req.URL.Query().Get("originals")) {
		err = r.AttachOriginals(req.Context(), a)
	}
	writeResult(w, a, err)
}

func (r *Resolver) handleSearch(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	n, err := counts(q, "max_results", "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a, err := r.SearchAnswer(req.Context(), SearchRequest{Query: q.Get("q"), Publisher: q.Get("publisher"), MaxResults: n[0], Offset: n[1]})
	if err == nil && wantsOriginals(q.Get("originals")) {
		err = r.AttachOriginals(req.Context(), a)
	}
	writeResult(w, a, err)
}

type verifyRequest struct {
	Passage   string `json:"passage"`
	URL       string `json:"url,omitempty"`
	Originals bool   `json:"originals,omitempty"`
}

func (r *Resolver) handleVerify(w http.ResponseWriter, req *http.Request) {
	var body verifyRequest
	if err := json.NewDecoder(io.LimitReader(req.Body, maxRequest)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errors.New(`want {"passage": "...", "url": "..."} with a passage`))
		return
	}
	a, err := r.VerifyAnswer(req.Context(), body.Passage, body.URL)
	if va, ok := a.(*VerifyAnswer); ok && err == nil && body.Originals {
		err = r.AttachOriginals(req.Context(), va)
	}
	writeResult(w, a, err)
}

// writeResult writes a signed answer, or the error as 400 for bad input and
// 502 for trouble reaching publishers.
func writeResult(w http.ResponseWriter, a any, err error) {
	switch {
	case errors.Is(err, ErrBadInput):
		writeError(w, http.StatusBadRequest, err)
	case err != nil:
		writeError(w, http.StatusBadGateway, err)
	default:
		writeJSON(w, http.StatusOK, a)
	}
}

func (r *Resolver) handleObject(w http.ResponseWriter, req *http.Request) {
	h := req.PathValue("hex")
	ctx := req.Context()
	if raw, rec, err := r.store.Record(ctx, core.RecordIDPrefix+h); err == nil && raw != nil {
		if _, ok := req.URL.Query()["bundle"]; ok {
			b, ok := r.store.Bundle(ctx, rec)
			if !ok {
				http.NotFound(w, req)
				return
			}
			writeJSON(w, http.StatusOK, b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(raw)
		return
	}
	if t, ok := r.store.ChunkText(core.ChunkIDPrefix + h); ok {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		io.WriteString(w, t)
		return
	}
	http.NotFound(w, req)
}

func (r *Resolver) handleChanges(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	limit := 500
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n < limit {
		limit = n
	}
	evs, err := r.store.Changes(req.Context(), since, q.Get("publisher"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	a := &ChangesAnswer{Changes: evs, Next: since}
	if len(evs) > 0 {
		a.Next = evs[len(evs)-1].Seq
	}
	a.AsOf = r.cfg.Now()
	writeResult(w, a, r.sign(a))
}

type announceRequest struct {
	Domain string `json:"domain"`
}

func (r *Resolver) handleAnnounce(w http.ResponseWriter, req *http.Request) {
	var body announceRequest
	if err := json.NewDecoder(io.LimitReader(req.Body, maxRequest)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errors.New(`want {"domain": "example.org"}`))
		return
	}
	if _, err := hostOf("https://" + body.Domain + "/"); err != nil || strings.ContainsAny(body.Domain, "/:") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("domain %q: want a bare domain", body.Domain))
		return
	}
	switch err := r.Announce(body.Domain); {
	case errors.Is(err, ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, err)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}

// AttachOriginals adds the publishers' signed records an answer relied on,
// as stored, and signs the answer again. Answers leave them out by default:
// a record lists every chunk of its page, so originals can be many times
// the size of the answer (a five-page search: about 160 KB of originals
// for 7 KB of results). Anyone checking an answer can instead fetch each
// record from its publisher by the ID the answer names.
func (r *Resolver) AttachOriginals(ctx context.Context, a signable) error {
	switch x := a.(type) {
	case *FetchAnswer:
		x.Originals = r.originals(ctx, x.Record)
	case *VerifyAnswer:
		x.Originals = r.originals(ctx, x.Record)
	case *ResolveAnswer:
		ids := []string{x.Current}
		if c, err := core.ParseCitation(x.Citation); err == nil {
			ids = append(ids, c.Record)
		}
		x.Originals = r.originals(ctx, ids...)
	case *SearchAnswer:
		var ids []string
		for _, res := range x.Results {
			ids = append(ids, res.Record)
		}
		x.Originals = r.originals(ctx, ids...)
	}
	return r.sign(a)
}

// wantsOriginals reports whether an HTTP caller asked for originals.
func wantsOriginals(v string) bool { return v == "1" || v == "true" }

// originals collects the stored original bytes of records by ID.
func (r *Resolver) originals(ctx context.Context, ids ...string) *Originals {
	o := &Originals{Records: map[string]json.RawMessage{}}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if raw, _, err := r.store.Record(ctx, id); err == nil && raw != nil {
			o.Records[id] = raw
		}
	}
	if len(o.Records) == 0 {
		return nil
	}
	return o
}

// sign names the resolver in an answer and signs it.
func (r *Resolver) sign(a signable) error {
	base := a.answer()
	base.VerifiedBy, base.ResolverSig = r.cfg.Name, nil
	base.AsOf = base.AsOf.UTC().Truncate(0)
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	b, err := core.CanonicalBytes(raw, "resolver_sig")
	if err != nil {
		return err
	}
	base.ResolverSig = core.SignBytes(r.keyID, r.priv, b)
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := core.EncodeJSON(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// hostOf returns the publisher of an https page URL on a domain.
func hostOf(pageURL string) (string, error) {
	u, err := url.Parse(pageURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" {
		return "", fmt.Errorf("url %q: want an https URL on a domain", pageURL)
	}
	return strings.ToLower(u.Hostname()), nil
}
