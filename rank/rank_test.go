package rank

import (
	"slices"
	"strings"
	"testing"
)

// page is a news story: a lead under the page's title, then sections.
var page = []Passage{
	{Section: []string{"Riverside"}, Text: "The bridge reopened on Monday after eight months of repairs and tests by engineers."},
	{Section: []string{"Riverside", "Cost"}, Text: "The repairs cost 2.4 million, under budget."},
	{Section: []string{"Riverside", "Traffic"}, Text: "Trucks over 7.5 tonnes remain banned from the bridge."},
	{Section: []string{"Riverside", "Reactions"}, Text: "Shop owners welcomed the reopening."},
}

func TestOrder(t *testing.T) {
	for _, tt := range []struct {
		query string
		want  []int
	}{
		{"are trucks banned", []int{2}},
		{"how much did the repairs cost", []int{1, 0}},
		{"bridge", []int{0, 2}},
		{"", []int{0, 1, 2, 3}},
		{"nothing matches here", []int{0, 1, 2, 3}},
		{"BRIDGE, Trucks!", []int{2, 0}},
	} {
		if got := Order(BM25, tt.query, page); !slices.Equal(got, tt.want) {
			t.Errorf("Order(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestLeadPrior(t *testing.T) {
	// "repairs" is in the lead and in the Cost section; the lead wins with
	// the prior, the shorter Cost passage without it.
	if got := Order(BM25, "repairs", page); got[0] != 1 {
		t.Errorf("BM25: %v, want the Cost passage first", got)
	}
	if got := Order(Lead, "repairs", page); got[0] != 0 {
		t.Errorf("Lead: %v, want the lead first", got)
	}
	if !slices.Equal(Order(nil, "repairs", page), Order(Default, "repairs", page)) {
		t.Error("a nil Ranker should be the default")
	}
	if Lead.Name() != "bm25-lead" || WithLeadPrior(Lead).Name() != "bm25-lead-lead" {
		t.Errorf("lead names: %s, %s", Lead.Name(), WithLeadPrior(Lead).Name())
	}
}

func TestNamed(t *testing.T) {
	for name, want := range map[string]string{"": Default.Name(), "bm25": "bm25", "bm25-lead": "bm25-lead"} {
		if r, err := Named(name); err != nil || r.Name() != want {
			t.Errorf("Named(%q) = %v, %v", name, r, err)
		}
	}
	if _, err := Named("embeddings"); err == nil {
		t.Error("unknown ranker accepted")
	}
}

func TestSlice(t *testing.T) {
	all := []int{5, 6, 7, 8, 9}
	for _, tt := range []struct {
		offset, k int
		want      []int
		more      bool
	}{
		{0, 2, []int{5, 6}, true},
		{2, 2, []int{7, 8}, true},
		{4, 2, []int{9}, false},
		{3, 2, []int{8, 9}, false},
		{5, 2, nil, false},
		{1, 0, []int{6, 7, 8, 9}, false},
	} {
		got, more := Slice(all, tt.offset, tt.k)
		if !slices.Equal(got, tt.want) || more != tt.more {
			t.Errorf("Slice(%d, %d) = %v, %v; want %v, %v", tt.offset, tt.k, got, more, tt.want, tt.more)
		}
	}
}

func TestTokens(t *testing.T) {
	got := Tokens("Café-owners' 2.4 million, ÜBER!")
	want := []string{"café", "owners", "2", "4", "million", "über"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestScoresEmpty(t *testing.T) {
	for _, r := range []Ranker{BM25, Lead} {
		if s := r.Scores("x", nil); len(s) != 0 {
			t.Fatal("no passages, no scores")
		}
		if s := r.Scores("x", []Passage{{}, {}}); s[0] != 0 || s[1] != 0 {
			t.Fatal("empty passages score zero")
		}
	}
}

func TestDocumentDropsLinkTargets(t *testing.T) {
	md := "[![](https://x.org/Land_on_the_Moon.jpg)](https://x.org/f) See [Apollo 11 in culture](https://x.org/wiki/Apollo \"t\") and https://x.org/raw."
	got := Document([]string{"Apollo 11"}, md)
	for _, gone := range []string{"Land_on_the_Moon", "x.org", "https"} {
		if strings.Contains(got, gone) {
			t.Errorf("Document kept %q: %q", gone, got)
		}
	}
	if !strings.Contains(got, "Apollo 11 in culture") || !strings.HasPrefix(got, "Apollo 11\n") {
		t.Errorf("Document lost visible text: %q", got)
	}
}

func TestTermsStemAndDropStopwords(t *testing.T) {
	got := terms("When did the crew land? They landed while landing, stopping briefly; many discoveries.")
	for _, w := range []string{"when", "did", "the", "they"} {
		if slices.Contains(got, w) {
			t.Errorf("stopword %q kept: %v", w, got)
		}
	}
	for _, w := range []string{"land", "stop", "brief", "discovery"} {
		if !slices.Contains(got, w) {
			t.Errorf("want %q in %v", w, got)
		}
	}
	if slices.Contains(got, "landed") || slices.Contains(got, "landing") {
		t.Errorf("suffixes not stripped: %v", got)
	}
}

func TestGroups(t *testing.T) {
	ps := []Passage{
		{Text: "Apollo 11 landed on the Moon in July 1969."}, // page 0
		{Text: "The crew trained for years."},                // page 0
		{Text: "Lake Baikal is the deepest lake."},           // page 1
		{Text: "The Moon landing was watched by millions."},  // page 2
	}
	group := []int{0, 0, 1, 2}
	got := Groups(BM25, "when did apollo land on the moon", ps, group)
	if len(got) != 2 || got[0].Group != 0 || !slices.Equal(got[0].Passages, []int{0}) || got[1].Group != 2 {
		t.Fatalf("got %+v", got)
	}
}
