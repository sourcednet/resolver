package passage

import (
	"slices"
	"strings"
	"testing"
)

const text = "The old stone bridge over the river reopened to traffic on Monday morning after eight months of repairs, and engineers replaced the worn mortar between the arches."

func TestWindows(t *testing.T) {
	if n := len(Windows(text)); n != len(strings.Fields(text))-WindowWords+1 {
		t.Fatalf("%d windows", n)
	}
	if !slices.Equal(Windows("The Bridge, reopened on Monday morning!"), Windows("the bridge reopened on monday morning")) {
		t.Fatal("case and punctuation should not matter")
	}
	if len(Windows("five words are too short")) != 0 {
		t.Fatal("passages under six words have no windows")
	}
}

// TestWinnowedFindsLongEnoughPassages checks winnowing's guarantee: every
// passage of MinWords words shares a stored window with its chunk.
func TestWinnowedFindsLongEnoughPassages(t *testing.T) {
	stored := Winnowed.Stored(text)
	if len(stored) >= len(All.Stored(text)) {
		t.Fatalf("winnowed stores %d of %d windows", len(stored), len(All.Stored(text)))
	}
	words := strings.Fields(text)
	for i := 0; i+Winnowed.MinWords() <= len(words); i++ {
		quote := strings.Join(words[i:i+Winnowed.MinWords()], " ")
		if !slices.ContainsFunc(Windows(quote), func(h int64) bool { return slices.Contains(stored, h) }) {
			t.Errorf("%q shares no stored window", quote)
		}
	}
}

func TestNamed(t *testing.T) {
	for name, want := range map[string]Index{"": Default, "windows": All, "winnowed": Winnowed} {
		if x, err := Named(name); err != nil || x != want {
			t.Errorf("Named(%q) = %v, %v", name, x, err)
		}
	}
	if _, err := Named("bloom"); err == nil {
		t.Error("unknown index accepted")
	}
}
