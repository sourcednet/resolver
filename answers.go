package resolver

import (
	"encoding/json"
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/verifier"
)

// Answer fields every resolver answer carries: who checked it, how fresh it
// is, and the resolver's signature over the whole answer.
type Answer struct {
	VerifiedBy  string          `json:"verified_by"`
	AsOf        time.Time       `json:"as_of"`
	ResolverSig *core.Signature `json:"resolver_sig,omitempty"`
}

func (a *Answer) answer() *Answer { return a }

// Originals are the publisher's signed records an answer relied on, exactly
// as the publisher served them, so anyone can check the answer later.
type Originals struct {
	Records map[string]json.RawMessage `json:"records"`
}

// FetchAnswer answers a fetch.
type FetchAnswer struct {
	verifier.Page
	Paging
	Originals *Originals `json:"originals,omitempty"`
	Answer
}

// Paging says where an answer's results sit in the full ranked list, so a
// client can ask for the next page.
type Paging struct {
	// Offset is how many results came before these.
	Offset int `json:"offset,omitempty"`
	// NextOffset is set when more results follow: pass it as offset to get
	// the next page.
	NextOffset int `json:"next_offset,omitempty"`
}

// ResolveAnswer answers a resolve.
type ResolveAnswer struct {
	verifier.Resolution
	Originals *Originals `json:"originals,omitempty"`
	Answer
}

// VerifyAnswer answers a verify with a URL.
type VerifyAnswer struct {
	verifier.PassageMatch
	Originals *Originals `json:"originals,omitempty"`
	Answer
}

// LookupAnswer answers a verify without a URL: which stored chunks contain
// the passage, and who published them.
type LookupAnswer struct {
	Passage string       `json:"passage"`
	Found   bool         `json:"found"`
	Matches []PassageHit `json:"matches"`
	// Note explains an empty result that isn't a real "no", such as a
	// passage too short to look up.
	Note string `json:"note,omitempty"`
	Answer
}

// ChangesAnswer is a page of the change feed.
type ChangesAnswer struct {
	Changes []ChangeEvent `json:"changes"`
	// Next is the cursor to pass as since for the following page.
	Next int64 `json:"next"`
	Answer
}

type signable interface{ answer() *Answer }
