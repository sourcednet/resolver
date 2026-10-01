// Package cmdutil holds what the resolver-side command-line tools share
// (sourced-resolver, and sourced-lab's demo): flag handling, logging
// flags, choosing the resolver's parts, and serving MCP next to the HTTP
// API.
package cmdutil

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sourcednet/resolver"
	"github.com/sourcednet/resolver/passage"
	"github.com/sourcednet/resolver/rank"
	"github.com/sourcednet/resolver/sourcedmcp"
)

// program is the running tool's name, for usage and error messages.
func program() string {
	return strings.TrimSuffix(filepath.Base(os.Args[0]), ".test")
}

// NewFlags returns a flag set that reports errors instead of exiting.
func NewFlags(name, args string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s %s [flags] %s\n\nFlags:\n", program(), name, args)
		fs.PrintDefaults()
	}
	return fs
}

// ParseFlags parses args and returns the exit code to use if it failed.
func ParseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

// Fail reports err and returns the exit code for a failed command.
func Fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "%s: %v\n", program(), err)
	return 1
}

// ListFlag collects a repeatable string flag.
type ListFlag []string

func (f *ListFlag) String() string     { return strings.Join(*f, ",") }
func (f *ListFlag) Set(s string) error { *f = append(*f, s); return nil }

// LogFlags are the logging flags shared by long-running commands.
type LogFlags struct {
	file    *string
	verbose *bool
}

// AddLogFlags adds -log and -v.
func AddLogFlags(fs *flag.FlagSet, fileHelp string) *LogFlags {
	return &LogFlags{
		file:    fs.String("log", "", fileHelp),
		verbose: fs.Bool("v", false, "verbose: also log every key, record, bundle, and HTTP request"),
	}
}

// Open returns a logger writing to stderr and, if set, to the log file
// (appended to; defaultFile when -log is empty). The returned func closes
// the file.
func (f *LogFlags) Open(stderr io.Writer, defaultFile string) (*slog.Logger, func(), error) {
	path := *f.file
	if path == "" {
		path = defaultFile
	}
	w, closeFn := stderr, func() {}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, err
		}
		w, closeFn = io.MultiWriter(stderr, file), func() { file.Close() }
	}
	level := slog.LevelInfo
	if *f.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})), closeFn, nil
}

// AddRankerFlag adds -ranker, which picks how passages are ordered for
// queries and search.
func AddRankerFlag(fs *flag.FlagSet) *string {
	return fs.String("ranker", rank.Default.Name(), fmt.Sprintf("how to order passages for queries and search: %s or %s", rank.BM25.Name(), rank.Lead.Name()))
}

// AddIndexFlag adds -index, which picks what the passage index stores.
func AddIndexFlag(fs *flag.FlagSet) *string {
	return fs.String("index", passage.Default.Name(), fmt.Sprintf("passage index: %s (every window) or %s (fewer windows: smaller, finds slightly fewer short quotes); changing it rebuilds the index in the background", passage.All.Name(), passage.Winnowed.Name()))
}

// MCPPath is where resolvers serve MCP.
const MCPPath = "/mcp"

// Handler serves a resolver's HTTP API and, at MCPPath, its MCP tools.
func Handler(r *resolver.Resolver, version string, log *slog.Logger) http.Handler {
	server := sourcedmcp.NewServer(r, "sourced-resolver", version, sourcedmcp.WithLogger(log))
	mux := http.NewServeMux()
	mux.Handle(MCPPath, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	mux.Handle("/", r.Handler())
	return mux
}
