package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// sourced runs the command in-process and returns its exit code and output.
func sourced(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// mustSourced runs the command and fails the test unless it succeeds.
func mustSourced(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := sourced(t, args...)
	if code != 0 {
		t.Fatalf("%s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}
