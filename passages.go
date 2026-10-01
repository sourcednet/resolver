package resolver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/passage"
	"github.com/sourcednet/resolver/rank"
)

// queueForIndex queues stored chunks for the passage index.
func queueForIndex(ctx context.Context, tx *sql.Tx, ids []string) error {
	for _, id := range ids {
		n, err := chunkNumber(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO index_queue(chunk) VALUES (?)`, n); err != nil {
			return err
		}
	}
	return nil
}

// indexBatch is how many chunks IndexPending indexes per transaction. The
// store has one connection, and a request waiting for it gets it after the
// current batch, so batches are kept to a few milliseconds. (With a second
// connection for the indexer, SQLite's busy handler kept requests' writes
// waiting until a whole page was indexed.)
const indexBatch = 4

// IndexPending adds every queued chunk to the passage index and reports
// how many it added. A resolver runs it in the background after storing new
// text, so answering a fetch never waits for indexing. Lookups run it
// first, so they cover every stored chunk.
func (s *Store) IndexPending(ctx context.Context) (int, error) {
	if s.readOnly {
		return 0, nil
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	done := 0
	for {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		batch, err := s.queued(ctx, indexBatch)
		if err != nil || len(batch) == 0 {
			return done, err
		}
		if err := s.addToIndex(ctx, batch); err != nil {
			return done, err
		}
		done += len(batch)
	}
}

type queuedChunk struct {
	n  int64
	id string
}

func (s *Store) queued(ctx context.Context, limit int) ([]queuedChunk, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT q.chunk, c.id FROM index_queue q JOIN chunks c ON c.n = q.chunk ORDER BY q.chunk LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queuedChunk
	for rows.Next() {
		var c queuedChunk
		if err := rows.Scan(&c.n, &c.id); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// addToIndex adds chunks' windows to the passage index and takes them off the
// queue, in one transaction. Text is read and hashed before it starts.
func (s *Store) addToIndex(ctx context.Context, batch []queuedChunk) error {
	var windows []any // hash, chunk, hash, chunk, …
	for _, c := range batch {
		if text, ok := s.ChunkText(c.id); ok {
			for _, h := range s.index.Stored(text) {
				windows = append(windows, h, c.n)
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for len(windows) > 0 {
		k := min(len(windows), 2*windowsPerInsert)
		q := `INSERT OR IGNORE INTO passage_windows(hash, chunk) VALUES ` + strings.TrimSuffix(strings.Repeat("(?, ?), ", k/2), ", ")
		if _, err := tx.ExecContext(ctx, q, windows[:k]...); err != nil {
			return err
		}
		windows = windows[k:]
	}
	for _, c := range batch {
		if _, err := tx.ExecContext(ctx, `DELETE FROM index_queue WHERE chunk = ?`, c.n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// windowsPerInsert is how many windows one INSERT statement adds; one row
// per statement spends most of the time on the statement, not the row.
const windowsPerInsert = 500

// unindex removes a chunk from the passage index, or from the queue if it
// wasn't indexed yet. Its windows are found by hashing its text again, so
// the index needs no second lookup by chunk. The caller holds indexMu.
func (s *Store) unindex(ctx context.Context, id string) error {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT n FROM chunks WHERE id = ?`, id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM index_queue WHERE chunk = ?`, n); err != nil {
		return err
	}
	if text, ok := s.ChunkText(id); ok {
		for _, h := range s.index.Stored(text) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM passage_windows WHERE hash = ? AND chunk = ?`, h, n); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Occurrence is a record that contains a matched chunk.
type Occurrence struct {
	Publisher   string    `json:"publisher"`
	URL         string    `json:"url"`
	Record      string    `json:"record"`
	PublishedAt time.Time `json:"published_at"`
	Current     bool      `json:"current"`
	Cite        string    `json:"cite"`
}

// PassageHit is a chunk that contains, or partly contains, a passage.
type PassageHit struct {
	Chunk string `json:"chunk"`
	Text  string `json:"text"`
	// Match is "exact" when every word of the passage appears in the chunk
	// in order (ignoring case, punctuation, and whitespace), otherwise "partial".
	Match string `json:"match"`
	// Overlap is the share of the passage's six-word windows found in the chunk.
	Overlap     float64      `json:"overlap"`
	Occurrences []Occurrence `json:"occurrences"`
}

// MinPassageWords is the shortest passage any passage index can look up.
// Some find only longer ones reliably (Store.MinPassageWords).
const MinPassageWords = passage.WindowWords

// MinPassageWords is the shortest passage the store's index always finds.
func (s *Store) MinPassageWords() int { return s.index.MinWords() }

// candidatesPerHit is how many chunks a lookup reads per result it
// returns: an index that stores fewer windows ranks candidates more
// roughly, so they are scored again on their full text.
const candidatesPerHit = 3

// LookupPassage finds stored chunks that share six-word windows with the
// passage, best first. Occurrences are ordered by claimed publication time,
// earliest first; in v1 that time is the publisher's claim, not a proof.
// Passages shorter than MinPassageWords find nothing.
func (s *Store) LookupPassage(ctx context.Context, quote string, limit int) ([]PassageHit, error) {
	hashes := passage.Windows(quote)
	if len(hashes) == 0 {
		return []PassageHit{}, nil
	}
	if _, err := s.IndexPending(ctx); err != nil {
		return nil, err
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")
	args := make([]any, 0, len(hashes)+1)
	for _, h := range hashes {
		args = append(args, h)
	}
	args = append(args, candidatesPerHit*limit)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT c.id, count(*) AS k FROM passage_windows w JOIN chunks c ON c.n = w.chunk
		 WHERE w.hash IN (%s) GROUP BY w.chunk ORDER BY k DESC, c.id LIMIT ?`, ph), args...)
	if err != nil {
		return nil, err
	}
	var cands []string
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	want := rank.Phrase(quote)
	hits := []PassageHit{}
	for _, c := range cands {
		text, ok := s.ChunkText(c)
		if !ok {
			continue
		}
		occ, err := s.occurrences(ctx, c)
		if err != nil {
			return nil, err
		}
		if len(occ) == 0 {
			continue
		}
		match := "partial"
		if strings.Contains(rank.Phrase(text), want) {
			match = "exact"
		}
		hits = append(hits, PassageHit{
			Chunk:       c,
			Text:        text,
			Match:       match,
			Overlap:     overlap(hashes, text),
			Occurrences: occ,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if (hits[i].Match == "exact") != (hits[j].Match == "exact") {
			return hits[i].Match == "exact"
		}
		return hits[i].Overlap > hits[j].Overlap
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// overlap is the share of a passage's windows that text holds.
func overlap(windows []int64, text string) float64 {
	held := map[int64]bool{}
	for _, h := range passage.Windows(text) {
		held[h] = true
	}
	n := 0
	for _, h := range windows {
		if held[h] {
			n++
		}
	}
	return float64(n) / float64(len(windows))
}

func (s *Store) occurrences(ctx context.Context, chunkID string) ([]Occurrence, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.publisher, r.url, r.published_at, cur.record_id IS NOT NULL
		FROM chunks k
		JOIN record_chunks rc ON rc.chunk = k.n
		JOIN records r ON r.n = rc.record
		LEFT JOIN current cur ON cur.url = r.url AND cur.record_id = r.id
		WHERE k.id = ? AND r.purged = 0
		ORDER BY r.published_at, r.id`, chunkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Occurrence
	for rows.Next() {
		var o Occurrence
		var at int64
		if err := rows.Scan(&o.Record, &o.Publisher, &o.URL, &at, &o.Current); err != nil {
			return nil, err
		}
		o.PublishedAt = fromMs(at)
		o.Cite = core.Citation{Publisher: o.Publisher, Record: o.Record, Chunk: chunkID}.String()
		out = append(out, o)
	}
	return out, rows.Err()
}
