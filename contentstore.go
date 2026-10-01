package resolver

import (
	"context"
	"io"
	"time"

	"github.com/sourcednet/core"
)

// ContentStore is everything a resolver keeps, in five parts that a store
// for another scale can implement separately. Store (SQLite plus chunk
// files) is the default.
type ContentStore interface {
	Objects
	PagePointers
	PublisherStates
	ChangeFeed
	Passages
	io.Closer
}

// Objects keeps verified, immutable objects: the publishers' signed
// records as served, and chunk text by ID. Text of a withdrawn page is
// purged and never stored again.
type Objects interface {
	// PutRecord stores a verified record's bytes; storing it again
	// replaces them (a re-signed record).
	PutRecord(ctx context.Context, raw []byte, r *core.Record) error
	// Record returns a stored record's bytes and decoded form, or nils.
	Record(ctx context.Context, id string) ([]byte, *core.Record, error)
	// PutBundle stores the text of a verified bundle's chunks.
	PutBundle(ctx context.Context, r *core.Record, b *core.Bundle) error
	// Bundle rebuilds a record's bundle if all its chunk text is stored.
	Bundle(ctx context.Context, r *core.Record) (*core.Bundle, bool)
	// ChunkText returns a chunk's stored text.
	ChunkText(id string) (string, bool)
	// Purge removes the text of every version before a withdrawal, keeping
	// chunks another live record uses, and the records' metadata.
	Purge(ctx context.Context, withdrawal *core.Record) (records, chunksDeleted int, err error)
}

// PagePointers tracks which record is current for each page, and when the
// publisher last confirmed it.
type PagePointers interface {
	Current(ctx context.Context, url string) (recordID string, checked time.Time, ok bool, err error)
	SetCurrent(ctx context.Context, publisher, url, recordID string, at time.Time) error
	ForgetCurrent(ctx context.Context, url string) error
	CurrentURLs(ctx context.Context, publisher string) ([]string, error)
	// Pages lists current pages, optionally one publisher's.
	Pages(ctx context.Context, publisher string) ([]PageInfo, error)
}

// PublisherStates keeps each publisher's sync state and last keys.json.
type PublisherStates interface {
	AddPublisher(ctx context.Context, domain string) (bool, error)
	Publisher(ctx context.Context, domain string) (*PublisherState, error)
	SavePublisher(ctx context.Context, p *PublisherState) error
	// DuePublishers returns the publishers whose next sync time has come.
	DuePublishers(ctx context.Context, now time.Time) ([]string, error)
	PutKeys(ctx context.Context, publisher string, raw []byte, at time.Time) error
	Keys(ctx context.Context, publisher string) ([]byte, time.Time, bool)
}

// ChangeFeed is the feed of new records the resolver has seen.
type ChangeFeed interface {
	AddChange(ctx context.Context, e ChangeEvent) error
	Changes(ctx context.Context, since int64, publisher string, limit int) ([]ChangeEvent, error)
}

// Passages finds which stored chunks hold a quoted passage.
type Passages interface {
	LookupPassage(ctx context.Context, quote string, limit int) ([]PassageHit, error)
	// PassageWindows says how many windows a passage has and how many the
	// index knows.
	PassageWindows(ctx context.Context, quote string) (total, known int, err error)
	// IndexPending indexes text stored but not yet indexed; the resolver
	// calls it in the background.
	IndexPending(ctx context.Context) (int, error)
	// MinPassageWords is the shortest passage always found.
	MinPassageWords() int
}

var _ ContentStore = (*Store)(nil)
