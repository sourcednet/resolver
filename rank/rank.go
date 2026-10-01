// Package rank orders a page's passages by relevance to a query. A Ranker
// scores passages; two are built in: BM25 (the default), and BM25 with a
// prior for the opening of a page. Resolvers and validating clients use the
// default unless configured otherwise, so they return the same passages for
// the same query.
package rank

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Passage is what a ranker sees of a chunk: its section path and its
// Markdown text.
type Passage struct {
	Section []string
	Text    string
}

// A Ranker scores passages for a query. Resolvers and validating clients
// take one in their configuration; Named finds the built-in ones.
type Ranker interface {
	// Name selects the ranker in configuration.
	Name() string
	// Scores returns each passage's score for query, higher is more
	// relevant; 0 means the passage shares no word with the query.
	Scores(query string, ps []Passage) []float64
}

var (
	// BM25 ranks passages by the query words they contain, weighted by how
	// rare each word is among the passages.
	BM25 Ranker = bm25Ranker{}
	// Lead is BM25 with a prior for the opening of a page (WithLeadPrior).
	// On the bench's Wikipedia questions it ranks answers higher than BM25,
	// but it can favor an opening paragraph that only mentions the query's
	// words over the section that answers it, as on a news page.
	Lead = WithLeadPrior(BM25)

	// Default is the ranker used unless configured otherwise.
	Default = BM25
)

// Named returns the built-in ranker called name; "" is Default.
func Named(name string) (Ranker, error) {
	if name == "" {
		return Default, nil
	}
	for _, r := range []Ranker{BM25, Lead} {
		if r.Name() == name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("unknown ranker %q (known: %s, %s)", name, BM25.Name(), Lead.Name())
}

// orDefault returns r, or Default if r is nil.
func orDefault(r Ranker) Ranker {
	if r == nil {
		return Default
	}
	return r
}

type bm25Ranker struct{}

func (bm25Ranker) Name() string                                { return "bm25" }
func (bm25Ranker) Scores(query string, ps []Passage) []float64 { return bm25(query, ps) }

// leadBoost is how much more a passage in a page's opening counts.
const leadBoost = 2

// WithLeadPrior wraps a ranker with a prior for the opening of a page:
// passages under no heading but the page's title, such as an article's
// lead and infobox, score double. Encyclopedias state their key facts
// first. Its name is the wrapped ranker's plus "-lead".
func WithLeadPrior(r Ranker) Ranker { return leadRanker{base: r} }

type leadRanker struct{ base Ranker }

func (l leadRanker) Name() string { return l.base.Name() + "-lead" }

func (l leadRanker) Scores(query string, ps []Passage) []float64 {
	scores := l.base.Scores(query, ps)
	for i, p := range ps {
		if len(p.Section) <= 1 {
			scores[i] *= leadBoost
		}
	}
	return scores
}

// Order ranks passages with r (Default if nil) and returns those that
// share a word with query, most relevant first; ties keep passage order. If
// none does, or the query is empty, it returns every passage in order.
func Order(r Ranker, query string, ps []Passage) []int {
	scores := orDefault(r).Scores(query, ps)
	var idx []int
	for i, s := range scores {
		if s > 0 {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		idx = make([]int, len(ps))
		for i := range idx {
			idx[i] = i
		}
		return idx
	}
	sort.SliceStable(idx, func(a, c int) bool { return scores[idx[a]] > scores[idx[c]] })
	return idx
}

// GroupHit is a group (such as a page) and its passages that share a word
// with the query, best first.
type GroupHit struct {
	Group    int
	Passages []int
}

// Groups ranks passages with r (Default if nil) across groups (group[i] is
// passage i's group) and returns every group with a passage that shares a
// word with query, ordered by its best passage.
func Groups(r Ranker, query string, ps []Passage, group []int) []GroupHit {
	scores := orDefault(r).Scores(query, ps)
	order := make([]int, 0, len(ps))
	for i, s := range scores {
		if s > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })
	var hits []GroupHit
	at := map[int]int{} // group → its index in hits
	for _, i := range order {
		g := group[i]
		k, ok := at[g]
		if !ok {
			k = len(hits)
			at[g] = k
			hits = append(hits, GroupHit{Group: g})
		}
		hits[k].Passages = append(hits[k].Passages, i)
	}
	return hits
}

// Slice returns one page of a ranked list: at most k items from offset on
// (k <= 0: all the rest), and whether more follow.
func Slice[T any](ranked []T, offset, k int) ([]T, bool) {
	if offset >= len(ranked) {
		return nil, false
	}
	rest := ranked[offset:]
	if k <= 0 || k >= len(rest) {
		return rest, false
	}
	return rest[:k], true
}

// BM25 parameters: the usual defaults.
const (
	k1 = 1.2
	b  = 0.75
)

// Tokens normalizes s (Unicode NFC, lowercase) and splits it into runs of
// letters and digits. Punctuation, markup, and whitespace fall away.
func Tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(norm.NFC.String(s)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

var (
	mdImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	bareURL = regexp.MustCompile(`https?://\S+`)
)

// Document is the text a chunk is ranked on: its section path and its
// visible text. Link targets, image addresses, and bare URLs are dropped,
// so words in file names and URLs don't count as content.
func Document(section []string, markdown string) string {
	text := mdImage.ReplaceAllString(markdown, "$1")
	text = mdLink.ReplaceAllString(text, "$1")
	text = bareURL.ReplaceAllString(text, " ")
	return strings.Join(section, " ") + "\n" + text
}

// Phrase returns s's tokens joined by single spaces, with a space at each
// end, so that strings.Contains(Phrase(text), Phrase(quote)) reports whether
// quote appears in text word for word, ignoring case, punctuation, and
// whitespace. An empty or punctuation-only s gives "".
func Phrase(s string) string {
	t := Tokens(s)
	if len(t) == 0 {
		return ""
	}
	return " " + strings.Join(t, " ") + " "
}

// stopwords are left out of ranking: they say nothing about relevance.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a about after all also an and any are as at be been before but by can could did
		do does for from had has have how i if in into is it its may more most no not of on or other our over she so
		such than that the their them then there these they this those through to under up was we were what when where
		which while who whom why will with would you your`) {
		stopwords[w] = true
	}
}

// terms are the words ranking compares: tokens without stopwords, each
// reduced to a rough stem, so "landed" and "landing" both match "land".
// Only ranking uses them; exact matching and the passage index compare
// words as they are.
func terms(s string) []string {
	var out []string
	for _, t := range Tokens(s) {
		if !stopwords[t] {
			out = append(out, stem(t))
		}
	}
	return out
}

// stem strips common English suffixes. It is deliberately light: a few
// rules that fix the commonest mismatches (plurals, -ed, -ing, -ly), not a
// full stemmer.
func stem(w string) string {
	if len(w) <= 4 {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "ing") && len(w) > 5:
		return undouble(w[:len(w)-3])
	case strings.HasSuffix(w, "ed") && len(w) > 4:
		return undouble(w[:len(w)-2])
	case strings.HasSuffix(w, "ly") && len(w) > 5:
		return w[:len(w)-2]
	case strings.HasSuffix(w, "es") && (strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes") || strings.HasSuffix(w, "xes")):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		return w[:len(w)-1]
	}
	return w
}

// undouble turns "stopp" into "stop" after a suffix is stripped.
func undouble(w string) string {
	if n := len(w); n >= 3 && w[n-1] == w[n-2] && !strings.ContainsRune("aeiouls", rune(w[n-1])) {
		return w[:n-1]
	}
	return w
}

// bm25 returns the BM25 score of each passage for query, with document
// frequencies taken from the passages themselves.
func bm25(query string, ps []Passage) []float64 {
	scores := make([]float64, len(ps))
	qterms := unique(terms(query))
	if len(qterms) == 0 || len(ps) == 0 {
		return scores
	}
	tf := make([]map[string]int, len(ps))
	lengths := make([]float64, len(ps))
	var total float64
	df := map[string]int{}
	for i, p := range ps {
		toks := terms(Document(p.Section, p.Text))
		lengths[i] = float64(len(toks))
		total += lengths[i]
		tf[i] = map[string]int{}
		for _, t := range toks {
			tf[i][t]++
		}
		for t := range tf[i] {
			df[t]++
		}
	}
	avg := total / float64(len(ps))
	if avg == 0 {
		return scores
	}
	n := float64(len(ps))
	for _, t := range qterms {
		if df[t] == 0 {
			continue
		}
		idf := math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
		for i := range ps {
			f := float64(tf[i][t])
			if f == 0 {
				continue
			}
			scores[i] += idf * f * (k1 + 1) / (f + k1*(1-b+b*lengths[i]/avg))
		}
	}
	return scores
}

func unique(ts []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range ts {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
