package main

import (
	"strings"
	"testing"
)

func TestServeFlags(t *testing.T) {
	if code, _, _ := sourced(t, "serve"); code != 2 {
		t.Fatalf("serve without -name: exit %d, want 2", code)
	}
	if code, _, errOut := sourced(t, "serve", "-name", "bad/name", "-data", t.TempDir(), "-addr", "127.0.0.1:0"); code != 1 || !strings.Contains(errOut, "bare domain") {
		t.Fatalf("bad name: exit %d, %s", code, errOut)
	}
	if code, _, errOut := sourced(t, "serve", "-name", "r.test", "-only", "-data", t.TempDir()); code != 1 || !strings.Contains(errOut, "-only needs") {
		t.Fatalf("-only without publishers: exit %d, %s", code, errOut)
	}
}
