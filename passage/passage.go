// Package passage decides what a resolver's passage index stores. The
// index maps hashes of six-word windows to the chunks that hold them, so a
// quoted passage can be traced to its source without a URL. An Index picks
// which of a chunk's windows to store; a lookup hashes every window of the
// passage, so any stored one can match.
package passage

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/sourcednet/resolver/rank"
)

// WindowWords is the number of words in each window. Six words are
// specific enough to identify a source and short enough that fragments,
// trimmed quotes, and lightly edited quotes still share windows with it.
const WindowWords = 6

// An Index chooses the windows a passage index stores for a chunk.
type Index interface {
	// Name selects the index in configuration.
	Name() string
	// Stored returns the hashes to store for a chunk's text.
	Stored(text string) []int64
	// MinWords is the shortest passage that is always found, wherever in a
	// stored chunk it appears.
	MinWords() int
}

var (
	// All stores every window: about 100 per chunk of 800 characters.
	All Index = all{}
	// Winnowed stores only the smallest hash of every 4 consecutive
	// windows, about 40% of them. On the bench it took a 22% smaller
	// database than All, but found 97% of quote fragments instead of 99.7%,
	// and passages need 9 words to be always found.
	Winnowed Index = winnowed{span: 4}

	// Default is the index used unless configured otherwise.
	Default = All
)

// Named returns the built-in index called name; "" is Default.
func Named(name string) (Index, error) {
	if name == "" {
		return Default, nil
	}
	for _, x := range []Index{All, Winnowed} {
		if x.Name() == name {
			return x, nil
		}
	}
	return nil, fmt.Errorf("unknown passage index %q (known: %s, %s)", name, All.Name(), Winnowed.Name())
}

// OrDefault returns x, or Default if x is nil.
func OrDefault(x Index) Index {
	if x == nil {
		return Default
	}
	return x
}

// Windows returns a hash for every window of text, after normalizing it
// (lowercase, letters and digits only), in order of first appearance and
// without repeats. Text shorter than WindowWords words has none.
func Windows(text string) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, h := range hashes(text) {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// hashes returns the hash of each window of text, in position order.
func hashes(text string) []int64 {
	toks := rank.Tokens(text)
	var out []int64
	for i := 0; i+WindowWords <= len(toks); i++ {
		sum := sha256.Sum256([]byte(strings.Join(toks[i:i+WindowWords], " ")))
		out = append(out, int64(binary.BigEndian.Uint64(sum[:8])))
	}
	return out
}

type all struct{}

func (all) Name() string               { return "windows" }
func (all) Stored(text string) []int64 { return Windows(text) }
func (all) MinWords() int              { return WindowWords }

// winnowed keeps the smallest hash of every span consecutive windows
// (Schleimer, Wilkerson, and Aiken's winnowing). Any passage of span
// windows inside a chunk contains at least one stored window.
type winnowed struct{ span int }

func (winnowed) Name() string    { return "winnowed" }
func (w winnowed) MinWords() int { return WindowWords + w.span - 1 }

func (w winnowed) Stored(text string) []int64 {
	hs := hashes(text)
	if len(hs) <= w.span {
		return Windows(text) // too short to thin out
	}
	var out []int64
	seen := map[int64]bool{}
	for i := 0; i+w.span <= len(hs); i++ {
		m := hs[i]
		for _, h := range hs[i+1 : i+w.span] {
			m = min(m, h)
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}
