package resolver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/passage"
	_ "modernc.org/sqlite"
)

// schema is the store's layout (storeVersion). Records and chunks are
// numbered (n), so the large tables, the passage index and which records
// contain which chunks, hold pairs of small integers instead of
// 71-character IDs.
const schema = `
CREATE TABLE IF NOT EXISTS publishers (
	domain       TEXT PRIMARY KEY,
	etag         TEXT    NOT NULL DEFAULT '',
	generated_at INTEGER NOT NULL DEFAULT 0,
	checked_at   INTEGER NOT NULL DEFAULT 0,
	synced_at    INTEGER NOT NULL DEFAULT 0,
	next_at      INTEGER NOT NULL DEFAULT 0,
	last_error   TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS records (
	n            INTEGER PRIMARY KEY,
	id           TEXT    NOT NULL UNIQUE,
	publisher    TEXT    NOT NULL,
	url          TEXT    NOT NULL,
	change       TEXT    NOT NULL DEFAULT '',
	supersedes   TEXT    NOT NULL DEFAULT '',
	published_at INTEGER NOT NULL,
	purged       INTEGER NOT NULL DEFAULT 0,
	raw          BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS records_url ON records(url);
CREATE TABLE IF NOT EXISTS chunks (
	n  INTEGER PRIMARY KEY,
	id TEXT    NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS record_chunks (
	chunk  INTEGER NOT NULL,
	record INTEGER NOT NULL,
	PRIMARY KEY (chunk, record)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS passage_windows (
	hash  INTEGER NOT NULL,
	chunk INTEGER NOT NULL,
	PRIMARY KEY (hash, chunk)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS index_queue (
	chunk INTEGER PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS current (
	url        TEXT PRIMARY KEY,
	publisher  TEXT    NOT NULL,
	record_id  TEXT    NOT NULL,
	checked_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS current_publisher ON current(publisher);
CREATE TABLE IF NOT EXISTS keys (
	publisher  TEXT PRIMARY KEY,
	raw        BLOB    NOT NULL,
	fetched_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS changes (
	seq       INTEGER PRIMARY KEY AUTOINCREMENT,
	publisher TEXT    NOT NULL,
	url       TEXT    NOT NULL,
	record_id TEXT    NOT NULL,
	change    TEXT    NOT NULL,
	seen_at   INTEGER NOT NULL
);
`

// Store keeps what a resolver has verified: records (as the publisher's
// signed originals) and metadata in SQLite, chunk text as content-addressed
// files. A chunk shared by several records or publishers is stored once.
type Store struct {
	db       *sql.DB
	dir      string
	index    passage.Index
	readOnly bool
	// indexMu serializes changes to the passage index: indexing queued
	// chunks, and removing purged ones.
	indexMu sync.Mutex
}

// OpenStore opens or creates a store in dir, upgrading an older layout.
// Its passage index is idx (passage.Default if nil); if the store's index
// was built by another, it is rebuilt in the background (IndexPending).
func OpenStore(dir string, idx passage.Index) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "resolver.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection: SQLite writes one at a time, and requests and the
	// background indexer take turns on it (see indexBatch).
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: dir, index: passage.OrDefault(idx)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store migration: %w", err)
	}
	if err := s.useIndex(); err != nil {
		db.Close()
		return nil, fmt.Errorf("passage index: %w", err)
	}
	return s, nil
}

// builtIndex returns the name of the passage index that built the store's
// windows. Stores from before the setting existed used every window.
func (s *Store) builtIndex() (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'passage_index'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return passage.All.Name(), nil
	}
	return name, err
}

// useIndex makes the store's windows those of its index: windows another
// index built are dropped, and every chunk is queued to be indexed again.
func (s *Store) useIndex() error {
	built, err := s.builtIndex()
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if built != s.index.Name() {
		if _, err := tx.Exec(`DELETE FROM passage_windows; INSERT OR IGNORE INTO index_queue(chunk) SELECT n FROM chunks`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('passage_index', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, s.index.Name()); err != nil {
		return err
	}
	return tx.Commit()
}

// storeVersion is the current layout of the store:
//   - 1 indexes passages by six-word windows (0 used whole sentences);
//   - 2 numbers chunks and records, keeping the passage index and the
//     record-to-chunk table about 10× smaller, and queues new text for
//     indexing instead of indexing it before answering.
const storeVersion = 2

// migrate brings an older store to the current layout. Records and the
// chunks they name are copied into the numbered tables; the passage index
// is dropped and every stored chunk is queued to be indexed again from its
// file (see IndexPending). Nothing is fetched again.
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= storeVersion {
		_, err := s.db.Exec(schema)
		return err
	}
	var old bool // the store holds data in an older layout
	if err := s.db.QueryRow(`SELECT count(*) > 0 FROM sqlite_schema WHERE type = 'table' AND name = 'record_chunks'`).Scan(&old); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if old {
		if _, err := tx.Exec(`
			ALTER TABLE records RENAME TO old_records;
			ALTER TABLE record_chunks RENAME TO old_record_chunks;
			DROP INDEX IF EXISTS records_url;
			DROP INDEX IF EXISTS record_chunks_chunk;
			DROP TABLE IF EXISTS shingles;
			DROP TABLE IF EXISTS sentences`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	if old {
		if _, err := tx.Exec(`
			INSERT INTO records(id, publisher, url, change, supersedes, published_at, purged, raw)
				SELECT id, publisher, url, change, supersedes, published_at, purged, raw FROM old_records ORDER BY rowid;
			INSERT OR IGNORE INTO chunks(id) SELECT chunk_id FROM old_record_chunks ORDER BY rowid;
			INSERT OR IGNORE INTO record_chunks(chunk, record)
				SELECT c.n, r.n FROM old_record_chunks o JOIN chunks c ON c.id = o.chunk_id JOIN records r ON r.id = o.record_id;
			DROP TABLE old_records;
			DROP TABLE old_record_chunks`); err != nil {
			return err
		}
		files, err := filepath.Glob(filepath.Join(s.dir, "chunks", "*", "*.md"))
		if err != nil {
			return err
		}
		ids := make([]string, len(files))
		for i, f := range files {
			ids[i] = core.ChunkIDPrefix + strings.TrimSuffix(filepath.Base(f), ".md")
		}
		if err := queueForIndex(context.Background(), tx, ids); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, storeVersion)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if old {
		_, err = s.db.Exec(`VACUUM`) // give the old index's space back
	}
	return err
}

// chunkNumber returns a chunk's number in the store, adding the chunk if
// it is new.
func chunkNumber(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT n FROM chunks WHERE id = ?`, id).Scan(&n)
	if !errors.Is(err, sql.ErrNoRows) {
		return n, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO chunks(id) VALUES (?)`, id)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Checkpoint folds SQLite's write-ahead log back into the database file, so
// file sizes reflect the data.
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMs(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// --- Publishers ------------------------------------------------------------

// PublisherState is what the store knows about syncing one publisher.
type PublisherState struct {
	Domain      string
	ETag        string
	GeneratedAt time.Time
	CheckedAt   time.Time
	SyncedAt    time.Time
	NextAt      time.Time
	LastError   string
}

// AddPublisher starts tracking a publisher. It reports whether it was new.
func (s *Store) AddPublisher(ctx context.Context, domain string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO publishers(domain) VALUES (?)`, strings.ToLower(domain))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Publisher returns a publisher's sync state, or nil if it isn't tracked.
func (s *Store) Publisher(ctx context.Context, domain string) (*PublisherState, error) {
	var p PublisherState
	var gen, checked, synced, next int64
	err := s.db.QueryRowContext(ctx,
		`SELECT domain, etag, generated_at, checked_at, synced_at, next_at, last_error FROM publishers WHERE domain = ?`,
		strings.ToLower(domain)).Scan(&p.Domain, &p.ETag, &gen, &checked, &synced, &next, &p.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.GeneratedAt, p.CheckedAt, p.SyncedAt, p.NextAt = fromMs(gen), fromMs(checked), fromMs(synced), fromMs(next)
	return &p, nil
}

// SavePublisher writes a publisher's sync state.
func (s *Store) SavePublisher(ctx context.Context, p *PublisherState) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE publishers SET etag = ?, generated_at = ?, checked_at = ?, synced_at = ?, next_at = ?, last_error = ? WHERE domain = ?`,
		p.ETag, ms(p.GeneratedAt), ms(p.CheckedAt), ms(p.SyncedAt), ms(p.NextAt), p.LastError, p.Domain)
	return err
}

// DuePublishers returns the publishers whose next sync time has come.
func (s *Store) DuePublishers(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT domain FROM publishers WHERE next_at <= ? ORDER BY next_at`, ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PutKeys stores a publisher's keys.json as last fetched.
func (s *Store) PutKeys(ctx context.Context, publisher string, raw []byte, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO keys(publisher, raw, fetched_at) VALUES (?, ?, ?)
		 ON CONFLICT(publisher) DO UPDATE SET raw = excluded.raw, fetched_at = excluded.fetched_at`,
		publisher, raw, ms(at))
	return err
}

// Keys returns a publisher's keys.json as last fetched, and when.
func (s *Store) Keys(ctx context.Context, publisher string) ([]byte, time.Time, bool) {
	var raw []byte
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT raw, fetched_at FROM keys WHERE publisher = ?`, publisher).Scan(&raw, &at)
	if err != nil {
		return nil, time.Time{}, false
	}
	return raw, fromMs(at), true
}

// --- Records and chunks ------------------------------------------------------

// PutRecord stores a verified record's original bytes. Storing a record
// again replaces its bytes, which keeps re-signed records current.
func (s *Store) PutRecord(ctx context.Context, raw []byte, r *core.Record) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE records SET raw = ? WHERE id = ?`, raw, r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return tx.Commit()
	}
	res, err = tx.ExecContext(ctx,
		`INSERT INTO records(id, publisher, url, change, supersedes, published_at, raw) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Publisher, r.URL, string(r.Change), r.Supersedes, ms(r.PublishedAt), raw)
	if err != nil {
		return err
	}
	rn, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, c := range r.Chunks {
		cn, err := chunkNumber(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO record_chunks(chunk, record) VALUES (?, ?)`, cn, rn); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Record returns a stored record's original bytes and decoded form. The
// bytes were verified when stored; callers that need a fresh guarantee
// verify them again.
func (s *Store) Record(ctx context.Context, id string) ([]byte, *core.Record, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT raw FROM records WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var r core.Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, nil, err
	}
	return raw, &r, nil
}

func (s *Store) purged(ctx context.Context, recordID string) bool {
	var p int
	err := s.db.QueryRowContext(ctx, `SELECT purged FROM records WHERE id = ?`, recordID).Scan(&p)
	return err == nil && p == 1
}

func (s *Store) chunkPath(id string) (string, error) {
	h, err := core.IDHex(id, core.ChunkIDPrefix)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, "chunks", h[:2], h+".md"), nil
}

// PutBundle stores the text of a verified bundle's chunks and queues new
// text for the passage index (see IndexPending). Text of a withdrawn record
// is never stored.
func (s *Store) PutBundle(ctx context.Context, r *core.Record, b *core.Bundle) error {
	if s.purged(ctx, r.ID) {
		return nil
	}
	var stored []string
	for _, c := range b.Chunks {
		p, err := s.chunkPath(c.ID)
		if err != nil {
			return err
		}
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(c.Text), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
		stored = append(stored, c.ID)
	}
	if len(stored) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := queueForIndex(ctx, tx, stored); err != nil {
		return err
	}
	return tx.Commit()
}

// ChunkText returns a chunk's stored text.
func (s *Store) ChunkText(id string) (string, bool) {
	p, err := s.chunkPath(id)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Bundle rebuilds a record's bundle from stored chunk text, if every chunk is present.
func (s *Store) Bundle(ctx context.Context, r *core.Record) (*core.Bundle, bool) {
	if s.purged(ctx, r.ID) {
		return nil, false
	}
	b := &core.Bundle{Record: r.ID, Chunks: make([]core.BundleChunk, len(r.Chunks))}
	for i, c := range r.Chunks {
		t, ok := s.ChunkText(c.ID)
		if !ok {
			return nil, false
		}
		b.Chunks[i] = core.BundleChunk{ID: c.ID, Text: t}
	}
	return b, true
}

// Purge removes the text of every version before a withdrawal: records are
// marked purged, and chunk files no other live record uses are deleted.
// Record metadata stays, as the spec requires.
func (s *Store) Purge(ctx context.Context, withdrawal *core.Record) (records, chunksDeleted int, err error) {
	var chunks []string
	for id, n := withdrawal.Supersedes, 0; id != "" && n < 10000; n++ {
		_, r, err := s.Record(ctx, id)
		if err != nil {
			return records, chunksDeleted, err
		}
		if r == nil {
			break
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE records SET purged = 1 WHERE id = ?`, id); err != nil {
			return records, chunksDeleted, err
		}
		records++
		for _, c := range r.Chunks {
			chunks = append(chunks, c.ID)
		}
		id = r.Supersedes
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	for _, c := range chunks {
		var live int
		if err := s.db.QueryRowContext(ctx, `
			SELECT count(*) FROM chunks k
			JOIN record_chunks rc ON rc.chunk = k.n
			JOIN records r ON r.n = rc.record
			WHERE k.id = ? AND r.purged = 0`, c).Scan(&live); err != nil {
			return records, chunksDeleted, err
		}
		if live > 0 {
			continue
		}
		if err := s.unindex(ctx, c); err != nil { // needs the text, so before deleting it
			return records, chunksDeleted, err
		}
		p, err := s.chunkPath(c)
		if err != nil {
			return records, chunksDeleted, err
		}
		if err := os.Remove(p); err == nil {
			chunksDeleted++
		} else if !errors.Is(err, fs.ErrNotExist) {
			return records, chunksDeleted, err
		}
	}
	return records, chunksDeleted, nil
}

// --- Current pointers ------------------------------------------------------

// Current returns the record a page currently points to and when that was
// last confirmed with the publisher.
func (s *Store) Current(ctx context.Context, url string) (string, time.Time, bool, error) {
	var id string
	var checked int64
	err := s.db.QueryRowContext(ctx, `SELECT record_id, checked_at FROM current WHERE url = ?`, url).Scan(&id, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	return id, fromMs(checked), err == nil, err
}

// SetCurrent points a page at a record, confirmed at time at.
func (s *Store) SetCurrent(ctx context.Context, publisher, url, recordID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO current(url, publisher, record_id, checked_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(url) DO UPDATE SET record_id = excluded.record_id, checked_at = excluded.checked_at`,
		url, publisher, recordID, ms(at))
	return err
}

// ForgetCurrent drops a page the publisher no longer lists.
func (s *Store) ForgetCurrent(ctx context.Context, url string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM current WHERE url = ?`, url)
	return err
}

// CurrentURLs lists the pages a publisher currently has.
func (s *Store) CurrentURLs(ctx context.Context, publisher string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT url FROM current WHERE publisher = ?`, publisher)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// --- Change feed -----------------------------------------------------------

// ChangeEvent is one entry of the change feed.
type ChangeEvent struct {
	Seq       int64     `json:"seq"`
	Publisher string    `json:"publisher"`
	URL       string    `json:"url"`
	Record    string    `json:"record"`
	Change    string    `json:"change"` // "new" or a record change type
	SeenAt    time.Time `json:"seen_at"`
}

// AddChange appends to the change feed.
func (s *Store) AddChange(ctx context.Context, e ChangeEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO changes(publisher, url, record_id, change, seen_at) VALUES (?, ?, ?, ?, ?)`,
		e.Publisher, e.URL, e.Record, e.Change, ms(e.SeenAt))
	return err
}

// Changes returns up to limit events after since, optionally for one publisher.
func (s *Store) Changes(ctx context.Context, since int64, publisher string, limit int) ([]ChangeEvent, error) {
	q := `SELECT seq, publisher, url, record_id, change, seen_at FROM changes WHERE seq > ?`
	args := []any{since}
	if publisher != "" {
		q += ` AND publisher = ?`
		args = append(args, strings.ToLower(publisher))
	}
	q += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChangeEvent{}
	for rows.Next() {
		var e ChangeEvent
		var seen int64
		if err := rows.Scan(&e.Seq, &e.Publisher, &e.URL, &e.Record, &e.Change, &seen); err != nil {
			return nil, err
		}
		e.SeenAt = fromMs(seen)
		out = append(out, e)
	}
	return out, rows.Err()
}
