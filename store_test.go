package resolver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sourcednet/core"
	"github.com/sourcednet/resolver/passage"
)

var ctx = context.Background()

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// put stores an unsigned record with the given chunk texts; the store
// trusts its callers to have verified what they put.
func put(t *testing.T, s *Store, id, pub, url, supersedes string, change core.Change, texts ...string) *core.Record {
	t.Helper()
	r := &core.Record{
		ID: core.RecordIDPrefix + strings.Repeat(id, 64), Publisher: pub, URL: url,
		PublishedAt: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC), Change: change,
	}
	if supersedes != "" {
		r.Supersedes = core.RecordIDPrefix + strings.Repeat(supersedes, 64)
	}
	b := &core.Bundle{Record: r.ID}
	for _, txt := range texts {
		r.Chunks = append(r.Chunks, core.ChunkRef{ID: core.ChunkID(txt)})
		b.Chunks = append(b.Chunks, core.BundleChunk{ID: core.ChunkID(txt), Text: txt})
	}
	raw, _ := json.Marshal(r)
	if err := s.PutRecord(ctx, raw, r); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBundle(ctx, r, b); err != nil {
		t.Fatal(err)
	}
	return r
}

const (
	shared  = "This paragraph was published by two different sites, word for word."
	private = "This paragraph only ever appeared on the first site, in its first version."
)

func TestPurgeKeepsSharedChunks(t *testing.T) {
	s := openTestStore(t)
	v1 := put(t, s, "a", "one.test", "https://one.test/p", "", "", shared, private)
	put(t, s, "b", "two.test", "https://two.test/q", "", "", shared)
	w := put(t, s, "c", "one.test", "https://one.test/p", "a", core.ChangeWithdrawal)

	records, chunks, err := s.Purge(ctx, w)
	if err != nil || records != 1 || chunks != 1 {
		t.Fatalf("purge: %d records, %d chunks, %v; want 1 and 1 (the shared chunk stays)", records, chunks, err)
	}
	if _, ok := s.ChunkText(core.ChunkID(private)); ok {
		t.Fatal("private chunk of a withdrawn page survived the purge")
	}
	if _, ok := s.ChunkText(core.ChunkID(shared)); !ok {
		t.Fatal("chunk still used by another publisher was deleted")
	}
	if _, ok := s.Bundle(ctx, v1); ok {
		t.Fatal("bundle of a purged record is still available")
	}
	if raw, _, _ := s.Record(ctx, v1.ID); raw == nil {
		t.Fatal("record metadata must stay after a purge")
	}
	// Text of a purged record never comes back.
	s.PutBundle(ctx, v1, &core.Bundle{Record: v1.ID, Chunks: []core.BundleChunk{{ID: core.ChunkID(private), Text: private}}})
	if _, ok := s.ChunkText(core.ChunkID(private)); ok {
		t.Fatal("purged text was stored again")
	}

	hits, err := s.LookupPassage(ctx, private, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("purged passage still found: %v, %v", hits, err)
	}
	hits, err = s.LookupPassage(ctx, shared, 10)
	if err != nil || len(hits) != 1 || len(hits[0].Occurrences) != 1 || hits[0].Occurrences[0].Publisher != "two.test" {
		t.Fatalf("shared passage: %+v, %v", hits, err)
	}
}

func TestLookupPassage(t *testing.T) {
	s := openTestStore(t)
	text := "The bridge reopened on Monday morning. The repairs cost 2.4 million, under budget. Trucks remain banned from crossing it."
	put(t, s, "a", "news.test", "https://news.test/bridge", "", "", text)

	for _, tt := range []struct {
		passage string
		match   string
	}{
		{"The repairs cost 2.4 million, under budget.", "exact"},
		{"the repairs  cost 2.4 million under budget", "exact"}, // case, punctuation, and spacing don't matter
		{"reopened on Monday morning. The repairs cost 2.4 million, under budget.", "exact"},
		{"cost 2.4 million, under budget. Trucks remain", "exact"}, // a fragment across sentences
		{"The repairs cost 3.1 million, over budget. Trucks remain banned from crossing it.", "partial"},
	} {
		hits, err := s.LookupPassage(ctx, tt.passage, 10)
		if err != nil || len(hits) != 1 || hits[0].Match != tt.match {
			t.Errorf("%q: got %+v (%v), want one %s match", tt.passage, hits, err, tt.match)
		}
	}
	if hits, _ := s.LookupPassage(ctx, "Something nobody ever wrote down anywhere at all.", 10); len(hits) != 0 {
		t.Fatalf("unrelated passage matched: %+v", hits)
	}
	if hits, _ := s.LookupPassage(ctx, "cost 2.4 million", 10); len(hits) != 0 {
		t.Fatal("passages shorter than the minimum should not match")
	}
}

// v1Tables is the part of the version 1 layout that version 2 changed.
const v1Tables = `
CREATE TABLE records (
	id TEXT PRIMARY KEY, publisher TEXT NOT NULL, url TEXT NOT NULL, change TEXT NOT NULL DEFAULT '',
	supersedes TEXT NOT NULL DEFAULT '', published_at INTEGER NOT NULL, purged INTEGER NOT NULL DEFAULT 0, raw BLOB NOT NULL);
CREATE INDEX records_url ON records(url);
CREATE TABLE record_chunks (record_id TEXT NOT NULL, pos INTEGER NOT NULL, chunk_id TEXT NOT NULL, PRIMARY KEY (record_id, pos));
CREATE INDEX record_chunks_chunk ON record_chunks(chunk_id);
CREATE TABLE shingles (hash INTEGER NOT NULL, chunk_id TEXT NOT NULL, PRIMARY KEY (hash, chunk_id)) WITHOUT ROWID;
CREATE INDEX shingles_chunk ON shingles(chunk_id);
PRAGMA user_version = 1;
`

func TestOldStoreIsUpgraded(t *testing.T) {
	dir := t.TempDir()
	text := "The bridge reopened on Monday morning after eight months of repairs."
	r := &core.Record{ID: core.RecordIDPrefix + strings.Repeat("a", 64), Publisher: "news.test", URL: "https://news.test/bridge",
		PublishedAt: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC), Chunks: []core.ChunkRef{{ID: core.ChunkID(text)}}}
	raw, _ := json.Marshal(r)

	// A version 1 store holding one record, its chunk file, and its windows.
	s, err := OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE records; DROP TABLE record_chunks; DROP TABLE passage_windows; DROP TABLE index_queue; DROP TABLE chunks;` + v1Tables); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO records(id, publisher, url, published_at, raw) VALUES (?, ?, ?, ?, ?)`, []any{r.ID, r.Publisher, r.URL, ms(r.PublishedAt), raw}},
		{`INSERT INTO record_chunks VALUES (?, 0, ?)`, []any{r.ID, r.Chunks[0].ID}},
		{`INSERT INTO shingles VALUES (1, ?)`, []any{r.Chunks[0].ID}},
	} {
		if _, err := s.db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := s.chunkPath(r.Chunks[0].ID)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, _, err := s.Record(ctx, r.ID); err != nil || string(got) != string(raw) {
		t.Fatalf("record after upgrade: %s, %v", got, err)
	}
	hits, err := s.LookupPassage(ctx, "reopened on Monday morning after eight months", 10)
	if err != nil || len(hits) != 1 || len(hits[0].Occurrences) != 1 || hits[0].Occurrences[0].Record != r.ID {
		t.Fatalf("lookup after upgrade: %+v, %v", hits, err)
	}
	var old int
	s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name IN ('shingles', 'old_records', 'old_record_chunks')`).Scan(&old)
	if old != 0 {
		t.Fatalf("%d tables of the old layout are left", old)
	}
}

func TestIndexingIsQueued(t *testing.T) {
	s := openTestStore(t)
	put(t, s, "a", "news.test", "https://news.test/bridge", "", "", "The bridge reopened on Monday morning after eight months of repairs.")
	if st, _ := s.Stats(ctx); st.Windows != 0 || st.Unindexed != 1 {
		t.Fatalf("before indexing: %d windows, %d queued; want 0 and 1", st.Windows, st.Unindexed)
	}
	if n, err := s.IndexPending(ctx); n != 1 || err != nil {
		t.Fatalf("IndexPending: %d, %v", n, err)
	}
	if st, _ := s.Stats(ctx); st.Windows == 0 || st.Unindexed != 0 {
		t.Fatalf("after indexing: %d windows, %d queued", st.Windows, st.Unindexed)
	}
}

func TestChangesFeed(t *testing.T) {
	s := openTestStore(t)
	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	for i, e := range []ChangeEvent{
		{Publisher: "one.test", URL: "https://one.test/a", Record: "r1", Change: "new"},
		{Publisher: "two.test", URL: "https://two.test/b", Record: "r2", Change: "new"},
		{Publisher: "one.test", URL: "https://one.test/a", Record: "r3", Change: "correction"},
	} {
		e.SeenAt = at.Add(time.Duration(i) * time.Minute)
		if err := s.AddChange(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.Changes(ctx, 0, "", 100)
	if len(all) != 3 || all[2].Change != "correction" {
		t.Fatalf("all changes: %+v", all)
	}
	after, _ := s.Changes(ctx, all[0].Seq, "", 100)
	one, _ := s.Changes(ctx, 0, "ONE.test", 100)
	if len(after) != 2 || len(one) != 2 || one[1].Record != "r3" {
		t.Fatalf("after %+v, one.test %+v", after, one)
	}
}

func TestPutRecordReplacesBytes(t *testing.T) {
	s := openTestStore(t)
	r := put(t, s, "a", "one.test", "https://one.test/p", "", "", "Some text long enough to matter here.")
	if err := s.PutRecord(ctx, []byte(`{"re-signed":true}`), r); err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.Record(ctx, r.ID)
	if err != nil || string(raw) != `{"re-signed":true}` {
		t.Fatalf("got %s, %v", raw, err)
	}
}

func TestChangingTheIndexRebuildsIt(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := "The old stone bridge over the river reopened to traffic on Monday morning after eight months of repairs."
	put(t, s, "a", "news.test", "https://news.test/bridge", "", "", text)
	s.IndexPending(ctx)
	full, _ := s.Stats(ctx)
	s.Close()

	s, err = OpenStore(dir, passage.Winnowed)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, _ := s.Stats(ctx); st.Index != "winnowed" || st.Windows != 0 || st.Unindexed != 1 {
		t.Fatalf("after switching: %s index, %d windows, %d queued", st.Index, st.Windows, st.Unindexed)
	}
	hits, err := s.LookupPassage(ctx, "reopened to traffic on Monday morning after eight months", 10)
	if err != nil || len(hits) != 1 || hits[0].Match != "exact" || hits[0].Overlap != 1 {
		t.Fatalf("lookup: %+v, %v", hits, err)
	}
	if st, _ := s.Stats(ctx); st.Windows == 0 || st.Windows >= full.Windows {
		t.Fatalf("winnowed stores %d windows, all windows %d", st.Windows, full.Windows)
	}
}
