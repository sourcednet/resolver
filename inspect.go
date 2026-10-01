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
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/passage"
)

// OpenStoreReadOnly opens an existing store for reading only, so a
// developer can look inside while a resolver is running on it.
func OpenStoreReadOnly(dir string) (*Store, error) {
	path := filepath.Join(dir, "resolver.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%s is not a resolver data directory: %w", dir, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		db.Close()
		return nil, err
	}
	if v < storeVersion {
		db.Close()
		return nil, fmt.Errorf("%s uses an older store layout (version %d, this tool reads %d): start a resolver on it once to upgrade it", dir, v, storeVersion)
	}
	return &Store{db: db, dir: dir, readOnly: true}, nil
}

// Stats summarizes what a store holds.
type Stats struct {
	Publishers   []PublisherStats
	Records      int
	Purged       int
	CurrentPages int
	Chunks       int
	ChunkRefs    int // (record, chunk) pairs; ChunkRefs/Chunks is the dedup ratio
	ChunkBytes   int64
	Windows      int
	Index        string // the passage index that built the windows
	Unindexed    int    // chunks queued for the passage index
	Changes      int
	DBBytes      int64
	// IndexBytes and RecordBytes are the parts of the database (after a
	// checkpoint) that the passage index and the signed records take.
	IndexBytes  int64
	RecordBytes int64
}

// PublisherStats is one publisher's sync state and page count.
type PublisherStats struct {
	PublisherState
	Pages int
}

// Stats counts what the store holds.
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	st := &Stats{}
	for q, dst := range map[string]*int{
		`SELECT count(*) FROM records`:              &st.Records,
		`SELECT count(*) FROM records WHERE purged`: &st.Purged,
		`SELECT count(*) FROM current`:              &st.CurrentPages,
		`SELECT count(*) FROM passage_windows`:      &st.Windows,
		`SELECT count(*) FROM index_queue`:          &st.Unindexed,
		`SELECT count(*) FROM changes`:              &st.Changes,
		`SELECT count(*) FROM record_chunks`:        &st.ChunkRefs,
	} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return nil, err
		}
	}
	var err error
	if st.Index, err = s.builtIndex(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT domain FROM publishers ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	var domains []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		domains = append(domains, d)
	}
	rows.Close()
	for _, d := range domains {
		p, err := s.Publisher(ctx, d)
		if err != nil || p == nil {
			return nil, err
		}
		ps := PublisherStats{PublisherState: *p}
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM current WHERE publisher = ?`, d).Scan(&ps.Pages); err != nil {
			return nil, err
		}
		st.Publishers = append(st.Publishers, ps)
	}
	filepath.WalkDir(filepath.Join(s.dir, "chunks"), func(_ string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			st.Chunks++
			if fi, err := e.Info(); err == nil {
				st.ChunkBytes += fi.Size()
			}
		}
		return nil
	})
	for q, dst := range map[string]*int64{
		`SELECT coalesce(sum(pgsize), 0) FROM dbstat WHERE name = 'passage_windows'`:                                         &st.IndexBytes,
		`SELECT coalesce(sum(pgsize), 0) FROM dbstat WHERE name IN ('records', 'records_url', 'sqlite_autoindex_records_1')`: &st.RecordBytes,
	} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"resolver.db", "resolver.db-wal"} {
		if fi, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			st.DBBytes += fi.Size()
		}
	}
	return st, nil
}

// PageInfo is a page the store currently points to a record for.
type PageInfo struct {
	URL       string
	Publisher string
	Record    string
	Title     string
	Change    string
	Chunks    int
	CheckedAt time.Time
}

// Pages lists current pages, optionally for one publisher.
func (s *Store) Pages(ctx context.Context, publisher string) ([]PageInfo, error) {
	q := `SELECT c.url, c.publisher, c.record_id, c.checked_at, r.change, r.raw
	      FROM current c LEFT JOIN records r ON r.id = c.record_id`
	var args []any
	if publisher != "" {
		q += ` WHERE c.publisher = ?`
		args = append(args, strings.ToLower(publisher))
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY c.url`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PageInfo
	for rows.Next() {
		var p PageInfo
		var checked int64
		var change sql.NullString
		var raw []byte
		if err := rows.Scan(&p.URL, &p.Publisher, &p.Record, &checked, &change, &raw); err != nil {
			return nil, err
		}
		p.CheckedAt, p.Change = fromMs(checked), change.String
		if r, err := decodeRecord(raw); err == nil {
			p.Title, p.Chunks = r.Title, len(r.Chunks)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FindRecord resolves a record ID, a unique prefix of its hex digest, or a
// page URL (its current record) to a full record ID.
func (s *Store) FindRecord(ctx context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, "https://") {
		id, _, ok, err := s.Current(ctx, ref)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no current record for %s", ref)
		}
		return id, nil
	}
	return s.findByPrefix(ctx, `SELECT id FROM records WHERE id LIKE ? LIMIT 2`, core.RecordIDPrefix, ref)
}

// FindChunk resolves a chunk ID or a unique prefix of its hex digest.
func (s *Store) FindChunk(ctx context.Context, ref string) (string, error) {
	return s.findByPrefix(ctx, `SELECT id FROM chunks WHERE id LIKE ? LIMIT 2`, core.ChunkIDPrefix, ref)
}

func (s *Store) findByPrefix(ctx context.Context, q, prefix, ref string) (string, error) {
	h := strings.TrimPrefix(ref, prefix)
	if h == "" || strings.Trim(h, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%q: want an ID or a hex prefix", ref)
	}
	rows, err := s.db.QueryContext(ctx, q, prefix+h+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("nothing stored matches %q", ref)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("%q is ambiguous; give more of the ID", ref)
}

// Occurrences lists the stored, unpurged records that contain a chunk.
func (s *Store) Occurrences(ctx context.Context, chunkID string) ([]Occurrence, error) {
	return s.occurrences(ctx, chunkID)
}

// Purged reports whether a record's text was purged after a withdrawal.
func (s *Store) Purged(ctx context.Context, recordID string) bool { return s.purged(ctx, recordID) }

// Unindexed returns how many stored chunks are waiting for the passage index.
func (s *Store) Unindexed(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM index_queue`).Scan(&n)
	return n, err
}

// PassageWindows returns how many six-word windows a passage has, and how
// many of them the index knows.
func (s *Store) PassageWindows(ctx context.Context, quote string) (total, known int, err error) {
	hs := passage.Windows(quote)
	for _, h := range hs {
		var found bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM passage_windows WHERE hash = ?)`, h).Scan(&found); err != nil {
			return 0, 0, err
		}
		if found {
			known++
		}
	}
	return len(hs), known, nil
}

func decodeRecord(raw []byte) (*core.Record, error) {
	if len(raw) == 0 {
		return nil, errors.New("no record")
	}
	var r core.Record
	return &r, json.Unmarshal(raw, &r)
}
