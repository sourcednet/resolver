package verifier

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/rank"
)

// Change is one newer version of a cited page.
type Change struct {
	Record      string      `json:"record"`
	Change      core.Change `json:"change"`
	Note        string      `json:"note,omitempty"`
	PublishedAt time.Time   `json:"published_at"`
}

// Resolution is what a verifier learned about a citation.
type Resolution struct {
	Verification Verification `json:"verification"`
	Reason       core.Reason  `json:"reason,omitempty"`
	Detail       string       `json:"detail,omitempty"`
	Citation     string       `json:"citation"`
	Publisher    string       `json:"publisher,omitempty"`
	URL          string       `json:"url,omitempty"`
	Title        string       `json:"title,omitempty"`
	// State is empty when the current version can't be determined.
	State core.State `json:"state,omitempty"`
	// Passage is the cited text, when the publisher still serves it.
	Passage *Chunk `json:"passage,omitempty"`
	// Unchanged means the cited passage is still in the current version.
	Unchanged bool `json:"unchanged"`
	// Latest is the current version of the passage when it changed: the
	// current chunk sharing the most words with it, if any is close enough.
	Latest  *Chunk   `json:"latest,omitempty"`
	Current string   `json:"current,omitempty"`
	Changes []Change `json:"changes,omitempty"`
}

// Resolve checks a citation and reports what has happened to the cited page
// since: current, revised, corrected, retracted, or withdrawn. It always
// asks the publisher for the current version live.
func (v *Verifier) Resolve(ctx context.Context, citation string) (*Resolution, error) {
	res := &Resolution{Citation: citation}
	fail := func(err error) (*Resolution, error) {
		r, d, err := failure(err)
		if err != nil {
			return nil, err
		}
		res.Verification, res.Reason, res.Detail = Failed, r, d
		return res, nil
	}

	c, err := core.ParseCitation(citation)
	if err != nil {
		return fail(&core.VerifyError{Reason: core.ReasonMalformed, Detail: err.Error()})
	}
	res.Publisher = c.Publisher
	cited, err := v.record(ctx, c.Publisher, c.Record)
	if err != nil {
		return fail(err)
	}
	if !slices.ContainsFunc(cited.Chunks, func(r core.ChunkRef) bool { return r.ID == c.Chunk }) {
		return fail(&core.VerifyError{Reason: core.ReasonHashMismatch, Detail: "cited chunk is not in the cited record"})
	}
	res.URL, res.Title, res.Verification = cited.URL, cited.Title, Verified

	// The cited passage, if the publisher still serves its text.
	if b, err := v.bundle(ctx, c.Publisher, cited); err == nil {
		res.Passage = chunkByID(chunksOf(cited, b), c.Chunk)
	} else if !errors.Is(err, ErrNotFound) {
		return fail(err)
	}

	currentID, err := v.currentRecordID(ctx, cited.URL)
	switch {
	case errors.Is(err, ErrNoRecord):
		res.Detail = "the publisher no longer lists this page, so its current state is unknown"
		return res, nil
	case err != nil:
		return fail(err)
	}
	res.Current = currentID
	if currentID == cited.ID {
		res.State, res.Unchanged = core.StateCurrent, true
		return res, nil
	}
	current, err := v.record(ctx, c.Publisher, currentID)
	if err != nil {
		return fail(err)
	}
	state, newer, err := core.ResolveState(cited.ID, current, func(id string) (*core.Record, error) {
		return v.record(ctx, c.Publisher, id)
	})
	if errors.Is(err, core.ErrNotInChain) {
		res.Detail = "the page's current version does not descend from the cited one"
		return res, nil
	}
	if err != nil {
		return fail(err)
	}
	res.State = state
	for _, r := range newer {
		res.Changes = append(res.Changes, Change{Record: r.ID, Change: r.Change, Note: r.Note, PublishedAt: r.PublishedAt})
	}
	if state == core.StateWithdrawn {
		res.Passage = nil // withdrawn text is never passed on, even from a cache
		return res, nil
	}

	b, err := v.bundle(ctx, c.Publisher, current)
	if err != nil {
		return fail(err)
	}
	now := chunksOf(current, b)
	if ch := chunkByID(now, c.Chunk); ch != nil {
		res.Unchanged, res.Latest = true, ch
		return res, nil
	}
	if res.Passage != nil {
		res.Latest = latestVersion(now, *res.Passage)
	}
	return res, nil
}

func chunkByID(cs []Chunk, id string) *Chunk {
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i]
		}
	}
	return nil
}

// minSimilarity is the least word overlap for a current chunk to count as
// the latest version of a cited passage; below it, nothing is offered.
const minSimilarity = 0.3

// latestVersion finds the current chunk most likely to be the cited
// passage's new version: the one sharing the most words with it (Jaccard
// similarity of their word sets), preferring the same section. It returns
// nil if nothing is similar enough.
func latestVersion(current []Chunk, cited Chunk) *Chunk {
	words := wordSet(cited.Text)
	var best *Chunk
	bestScore := minSimilarity
	for i := range current {
		score := jaccard(words, wordSet(current[i].Text))
		if slices.Equal(current[i].Section, cited.Section) {
			score += 0.1 // a tie-breaker, not a requirement: sections get renamed too
		}
		if score > bestScore {
			best, bestScore = &current[i], score
		}
	}
	return best
}

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, t := range rank.Tokens(s) {
		m[t] = true
	}
	return m
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// PassageMatch is the result of checking a passage against a page.
type PassageMatch struct {
	Verification Verification `json:"verification"`
	Reason       core.Reason  `json:"reason,omitempty"`
	Detail       string       `json:"detail,omitempty"`
	Publisher    string       `json:"publisher,omitempty"`
	URL          string       `json:"url"`
	Record       string       `json:"record,omitempty"`
	Found        bool         `json:"found"`
	Chunk        *Chunk       `json:"chunk,omitempty"`
}

// VerifyPassage checks whether passage appears in the signed content of the
// page at pageURL, word for word. Matching ignores case, punctuation, and
// whitespace, so a quote with different quote marks or line breaks still
// matches, but a changed word does not.
func (v *Verifier) VerifyPassage(ctx context.Context, pageURL, passage string) (*PassageMatch, error) {
	p, err := v.Fetch(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	return MatchPassage(p, passage), nil
}

// MatchPassage checks whether passage appears in a fetched page, the same
// way VerifyPassage does.
func MatchPassage(p *Page, passage string) *PassageMatch {
	m := &PassageMatch{Verification: p.Verification, Reason: p.Reason, Detail: p.Detail, Publisher: p.Publisher, URL: p.URL, Record: p.Record}
	want := rank.Phrase(passage)
	if p.Verification != Verified || want == "" {
		return m
	}
	for i := range p.Chunks {
		if strings.Contains(rank.Phrase(p.Chunks[i].Text), want) {
			m.Found, m.Chunk = true, &p.Chunks[i]
			break
		}
	}
	return m
}
