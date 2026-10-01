package main

import (
	"bytes"
	"context"
	"github.com/sourcednet/testkit/testnet"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sourcednet/resolver"
)

func TestInspectAndLogs(t *testing.T) {
	n := testnet.Standard(t)
	dir := t.TempDir()
	var logs lockedBuffer // the background indexer logs too
	r, err := resolver.New(resolver.Config{Name: "resolver.test", DataDir: dir, HTTP: n.Client(), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() // inspect reads while the resolver still has the store open
	for _, p := range []string{"daily-herald.test", "longform.test"} {
		if err := r.Sync(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.FetchAnswer(context.Background(), resolver.FetchRequest{URL: "https://daily-herald.test/news/2026/09/bridge-reopens.html"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store().IndexPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"msg=sync publisher=daily-herald.test result=updated", "msg=change", "msg=page url=https://daily-herald.test/news/2026/09/bridge-reopens.html mode=live verification=verified"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"inspect", "-data", dir}, "daily-herald.test  3"},
		{[]string{"inspect", "-data", dir, "pages", "longform.test"}, "https://longform.test/essays/keeping-things.html"},
		{[]string{"inspect", "-data", dir, "record", "https://daily-herald.test/news/2026/09/bridge-reopens.html"}, "Current    yes"},
		{[]string{"inspect", "-data", dir, "search", "the archive grew in ways its founders had not planned for"}, "exact match"},
		{[]string{"inspect", "-data", dir, "changes"}, "new"},
	} {
		if out := mustSourced(t, tt.args...); !strings.Contains(out, tt.want) {
			t.Errorf("sourced %s: output lacks %q:\n%s", strings.Join(tt.args, " "), tt.want, out)
		}
	}

	if code, _, errOut := sourced(t, "inspect", "-data", dir, "record", "ffffffff"); code != 1 || !strings.Contains(errOut, "nothing stored matches") {
		t.Fatalf("unknown record: exit %d, %s", code, errOut)
	}
}

// lockedBuffer is a bytes.Buffer safe for concurrent writes and reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
