package resolver

import (
	"context"
	"strings"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/rank"
	"github.com/sourcednet/resolver/verifier"
)

// DefaultSearchResults is how many pages a search returns by default.
const DefaultSearchResults = 5

// TopResultPassages is how many passages the first result of a search
// answer carries. The first page is usually the right one (96% on the
// bench), but its best passage holds the answer only about half the time,
// so a few more often save a fetch.
const TopResultPassages = 3

// SearchResult is one page found by a search, with its best passage.
type SearchResult struct {
	Publisher string         `json:"publisher"`
	URL       string         `json:"url"`
	Title     string         `json:"title"`
	Record    string         `json:"record"`
	Passage   verifier.Chunk `json:"passage"`
	// MorePassages are the page's next best passages, best first, on the
	// answer's first result only (up to TopResultPassages in all).
	MorePassages []verifier.Chunk `json:"more_passages,omitempty"`
}

// SearchAnswer answers a search: pages this resolver holds that match the
// query, best first.
type SearchAnswer struct {
	Query     string         `json:"query"`
	Publisher string         `json:"publisher,omitempty"`
	Results   []SearchResult `json:"results"`
	Paging
	Originals *Originals `json:"originals,omitempty"`
	Answer
}

// SearchRequest asks for pages that match a query.
type SearchRequest struct {
	Query string
	// Publisher, if set, limits the search to that publisher's pages.
	Publisher string
	// MaxResults is how many pages to return; 0 means DefaultSearchResults.
	MaxResults int
	// Offset skips that many pages first, for the next page of results.
	Offset int
}

// SearchAnswer searches the current pages this resolver holds and returns
// the best ones with their best passage. It searches what the resolver has
// synced or fetched, not the web: an AI app uses it when it has no URL.
// Everything returned was verified when it was stored; see AttachOriginals
// for checking it later.
func (r *Resolver) SearchAnswer(ctx context.Context, req SearchRequest) (*SearchAnswer, error) {
	if strings.TrimSpace(req.Query) == "" {
		return nil, badInput("missing query")
	}
	if req.MaxResults < 0 || req.Offset < 0 {
		return nil, badInput("max_results and offset: want non-negative numbers")
	}
	if req.MaxResults == 0 {
		req.MaxResults = DefaultSearchResults
	}
	pages, err := r.store.Pages(ctx, req.Publisher)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		page  PageInfo
		rec   *core.Record
		chunk core.BundleChunk
		index int
	}
	var cands []candidate
	var passages []rank.Passage
	var group []int
	for gi, pg := range pages {
		_, rec, err := r.store.Record(ctx, pg.Record)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.Change == core.ChangeWithdrawal {
			continue
		}
		b, ok := r.store.Bundle(ctx, rec)
		if !ok {
			continue // text not stored (yet); a fetch of the page brings it in
		}
		for i, c := range b.Chunks {
			cands = append(cands, candidate{page: pg, rec: rec, chunk: c, index: i})
			passages = append(passages, rank.Passage{Section: rec.Chunks[i].Section, Text: c.Text})
			group = append(group, gi)
		}
	}

	a := &SearchAnswer{Query: req.Query, Publisher: strings.ToLower(req.Publisher), Results: []SearchResult{}}
	hits, more := rank.Slice(rank.Groups(r.cfg.Ranker, req.Query, passages, group), req.Offset, req.MaxResults)
	chunk := func(i int) verifier.Chunk {
		c := cands[i]
		return verifier.Chunk{
			ID:      c.chunk.ID,
			Section: c.rec.Chunks[c.index].Section,
			Text:    c.chunk.Text,
			Cite:    core.Citation{Publisher: c.rec.Publisher, Record: c.rec.ID, Chunk: c.chunk.ID}.String(),
		}
	}
	for n, hit := range hits {
		c := cands[hit.Passages[0]]
		res := SearchResult{Publisher: c.rec.Publisher, URL: c.page.URL, Title: c.rec.Title, Record: c.rec.ID, Passage: chunk(hit.Passages[0])}
		if n == 0 {
			more, _ := rank.Slice(hit.Passages, 1, TopResultPassages-1)
			for _, i := range more {
				res.MorePassages = append(res.MorePassages, chunk(i))
			}
		}
		a.Results = append(a.Results, res)
	}
	a.Offset = req.Offset
	if more {
		a.NextOffset = req.Offset + len(hits)
	}
	r.log.Info("search", "query", req.Query, "publisher", req.Publisher, "offset", req.Offset, "pages", len(pages), "passages", len(passages), "results", len(a.Results))
	a.AsOf = r.cfg.Now()
	return a, r.sign(a)
}
