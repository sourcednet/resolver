package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/sourcednet/resolver/cmdutil"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sourcednet/resolver"
	"github.com/sourcednet/resolver/rank"
)

const inspectUsage = `Usage: sourced-resolver inspect [flags] [view] [argument]

Shows what a resolver has stored, read-only (safe while it runs).

Views:
  (none)                 summary: publishers, sync state, counts, disk use
  pages [publisher]      current pages and their records
  record <id|url>        a record: metadata, chunks, history (-raw for its JSON)
  chunk <id>             a chunk's text and the records that contain it
  changes                the change feed (-since N to page through it)
  search "<passage>"     what the passage index matches, and why

IDs can be shortened to a unique prefix, as in the logs.

Flags:
`

func cmdInspect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := cmdutil.NewFlags("inspect", "[view] [argument]", stderr)
	fs.Usage = func() { fmt.Fprint(stderr, inspectUsage); fs.PrintDefaults() }
	data := fs.String("data", "resolver-data", "resolver data directory, or a demo directory (uses its .resolver)")
	raw := fs.Bool("raw", false, "record view: print the record's signed JSON as stored")
	since := fs.Int64("since", 0, "changes view: show changes after this sequence number")
	if code, ok := cmdutil.ParseFlags(fs, args); !ok {
		return code
	}
	dir := *data
	if _, err := os.Stat(filepath.Join(dir, ".resolver", "resolver.db")); err == nil {
		dir = filepath.Join(dir, ".resolver")
	}
	s, err := resolver.OpenStoreReadOnly(dir)
	if err != nil {
		return cmdutil.Fail(stderr, err)
	}
	defer s.Close()

	view, arg := fs.Arg(0), strings.Join(fs.Args()[min(1, fs.NArg()):], " ")
	switch view {
	case "", "summary":
		err = inspectSummary(ctx, stdout, s, dir)
	case "pages":
		err = inspectPages(ctx, stdout, s, arg)
	case "record":
		err = inspectRecord(ctx, stdout, s, arg, *raw)
	case "chunk":
		err = inspectChunk(ctx, stdout, s, arg)
	case "changes":
		err = inspectChanges(ctx, stdout, s, *since)
	case "search":
		err = inspectSearch(ctx, stdout, s, arg)
	default:
		fs.Usage()
		return 2
	}
	if err != nil {
		return cmdutil.Fail(stderr, err)
	}
	return 0
}

func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func when(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func inspectSummary(ctx context.Context, w io.Writer, s *resolver.Store, dir string) error {
	st, err := s.Stats(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Resolver data in %s (database %s, of which passage index %s and signed records %s; chunk text %s)\n\n",
		dir, size(st.DBBytes), size(st.IndexBytes), size(st.RecordBytes), size(st.ChunkBytes))
	t := table(w)
	fmt.Fprintln(t, "PUBLISHER\tPAGES\tLAST SYNC\tNEXT SYNC\tMANIFEST\tETAG\tLAST ERROR")
	for _, p := range st.Publishers {
		gen := "-"
		if !p.GeneratedAt.IsZero() {
			gen = p.GeneratedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(t, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n", p.Domain, p.Pages, when(p.SyncedAt), when(p.NextAt), gen, orDash(p.ETag), oneLine(orDash(p.LastError), 60))
	}
	t.Flush()
	fmt.Fprintf(w, "\n%d records (%d with text purged), %d current pages, %d chunks, %d index windows (%s index, %d chunks waiting), %d changes\n",
		st.Records, st.Purged, st.CurrentPages, st.Chunks, st.Windows, st.Index, st.Unindexed, st.Changes)
	return nil
}

func inspectPages(ctx context.Context, w io.Writer, s *resolver.Store, publisher string) error {
	pages, err := s.Pages(ctx, publisher)
	if err != nil {
		return err
	}
	t := table(w)
	fmt.Fprintln(t, "URL\tRECORD\tCHANGE\tCHUNKS\tCHECKED\tTITLE")
	for _, p := range pages {
		change := p.Change
		if change == "" {
			change = "first"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%d\t%s\t%s\n", p.URL, resolver.Short(p.Record), change, p.Chunks, when(p.CheckedAt), oneLine(p.Title, 40))
	}
	t.Flush()
	fmt.Fprintf(w, "\n%d pages\n", len(pages))
	return nil
}

func inspectRecord(ctx context.Context, w io.Writer, s *resolver.Store, ref string, raw bool) error {
	if ref == "" {
		return errors.New("record: give an ID, a prefix, or a page URL")
	}
	id, err := s.FindRecord(ctx, ref)
	if err != nil {
		return err
	}
	b, r, err := s.Record(ctx, id)
	if err != nil {
		return err
	}
	if raw {
		_, err := w.Write(b)
		fmt.Fprintln(w)
		return err
	}
	fmt.Fprintf(w, "Record     %s\n", id)
	fmt.Fprintf(w, "URL        %s\n", r.URL)
	fmt.Fprintf(w, "Title      %s\n", r.Title)
	fmt.Fprintf(w, "Publisher  %s (signed with key %s)\n", r.Publisher, r.Sig.KeyID)
	fmt.Fprintf(w, "Published  %s (the publisher's claim)\n", r.PublishedAt.UTC().Format(time.RFC3339))
	if r.Change != "" {
		fmt.Fprintf(w, "Change     %s of %s", r.Change, resolver.Short(r.Supersedes))
		if r.Note != "" {
			fmt.Fprintf(w, ": %q", r.Note)
		}
		fmt.Fprintln(w)
	}
	cur, checked, ok, err := s.Current(ctx, r.URL)
	switch {
	case err != nil:
		return err
	case ok && cur == id:
		fmt.Fprintf(w, "Current    yes, last confirmed %s\n", when(checked))
	case ok:
		fmt.Fprintf(w, "Current    no; the page's current record is %s\n", resolver.Short(cur))
	default:
		fmt.Fprintln(w, "Current    no; the page is not listed now")
	}
	if s.Purged(ctx, id) {
		fmt.Fprintln(w, "Text       purged after a withdrawal; only this metadata remains")
	}

	if len(r.Chunks) > 0 {
		fmt.Fprintln(w, "\nChunks")
		t := table(w)
		fmt.Fprintln(t, "#\tCHUNK\tCHARS\tSECTION\tSTARTS WITH")
		for i, c := range r.Chunks {
			text, stored := s.ChunkText(c.ID)
			chars, start := "-", "(not stored)"
			if stored {
				chars, start = fmt.Sprint(len([]rune(text))), oneLine(text, 50)
			}
			fmt.Fprintf(t, "%d\t%s\t%s\t%s\t%s\n", i+1, resolver.Short(c.ID), chars, oneLine(strings.Join(c.Section, " › "), 36), start)
		}
		t.Flush()
	}

	fmt.Fprintln(w, "\nHistory (this record and what it supersedes)")
	t := table(w)
	for hid, n := id, 0; hid != "" && n < 100; n++ {
		_, h, err := s.Record(ctx, hid)
		if err != nil {
			return err
		}
		if h == nil {
			fmt.Fprintf(t, "%s\t(not stored)\n", resolver.Short(hid))
			break
		}
		change := string(h.Change)
		if change == "" {
			change = "first version"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", resolver.Short(h.ID), change, h.PublishedAt.UTC().Format("2006-01-02"), orDash(h.Note))
		hid = h.Supersedes
	}
	return t.Flush()
}

func inspectChunk(ctx context.Context, w io.Writer, s *resolver.Store, ref string) error {
	if ref == "" {
		return errors.New("chunk: give an ID or a prefix")
	}
	id, err := s.FindChunk(ctx, ref)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Chunk  %s\n", id)
	text, ok := s.ChunkText(id)
	if ok {
		total, _, err := s.PassageWindows(ctx, text)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "Size   %d characters, %d words, %d index windows\n", len([]rune(text)), len(rank.Tokens(text)), total)
	} else {
		fmt.Fprintln(w, "Text   not stored (purged after a withdrawal)")
	}
	occ, err := s.Occurrences(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\nIn %d stored records\n", len(occ))
	t := table(w)
	for _, o := range occ {
		cur := ""
		if o.Current {
			cur = "current"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", resolver.Short(o.Record), o.URL, o.PublishedAt.UTC().Format("2006-01-02"), cur)
	}
	t.Flush()
	if ok {
		fmt.Fprintf(w, "\n%s\n", text)
	}
	return nil
}

func inspectChanges(ctx context.Context, w io.Writer, s *resolver.Store, since int64) error {
	evs, err := s.Changes(ctx, since, "", 500)
	if err != nil {
		return err
	}
	t := table(w)
	fmt.Fprintln(t, "SEQ\tSEEN\tCHANGE\tRECORD\tURL")
	for _, e := range evs {
		fmt.Fprintf(t, "%d\t%s\t%s\t%s\t%s\n", e.Seq, when(e.SeenAt), e.Change, resolver.Short(e.Record), e.URL)
	}
	t.Flush()
	if len(evs) == 500 {
		fmt.Fprintf(w, "\nMore: sourced-resolver inspect -since %d changes\n", evs[len(evs)-1].Seq)
	}
	return nil
}

func inspectSearch(ctx context.Context, w io.Writer, s *resolver.Store, passage string) error {
	if strings.TrimSpace(passage) == "" {
		return errors.New(`search: give a passage, e.g. sourced-resolver inspect search "the archive grew in ways"`)
	}
	words := len(rank.Tokens(passage))
	total, known, err := s.PassageWindows(ctx, passage)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Passage: %d words, %d six-word windows, %d of them in the index\n", words, total, known)
	if n, err := s.Unindexed(ctx); err == nil && n > 0 {
		fmt.Fprintf(w, "%d stored chunks are still waiting to be indexed, so results may be incomplete.\n", n)
	}
	if words < resolver.MinPassageWords {
		fmt.Fprintf(w, "Too short to look up: the index needs at least %d words.\n", resolver.MinPassageWords)
		return nil
	}
	hits, err := s.LookupPassage(ctx, passage, 10)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Fprintln(w, "No stored chunk shares a window with it.")
		return nil
	}
	for i, h := range hits {
		fmt.Fprintf(w, "\n[%d] %s match, %.0f%% of windows, chunk %s\n", i+1, h.Match, h.Overlap*100, resolver.Short(h.Chunk))
		for _, o := range h.Occurrences {
			cur := ""
			if o.Current {
				cur = " (current)"
			}
			fmt.Fprintf(w, "    %s  %s%s\n", resolver.Short(o.Record), o.URL, cur)
		}
		fmt.Fprintf(w, "    %s\n", oneLine(h.Text, 160))
	}
	return nil
}
