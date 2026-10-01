package main

import (
	"context"
	"github.com/sourcednet/resolver/cmdutil"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sourcednet/resolver/httplog"
	"github.com/sourcednet/resolver/rank"
	"github.com/sourcednet/resolver/sourcedmcp"
	"github.com/sourcednet/resolver/verifier"
)

// cmdMCP runs the validating client: the sourced tools over stdio, checking
// every signature locally against publishers' own keys.
func cmdMCP(ctx context.Context, args []string, _, stderr io.Writer) int {
	fs := cmdutil.NewFlags("mcp", "", stderr)
	ca := fs.String("ca", "", "dev only: also trust this PEM certificate authority when reaching publishers")
	logs := cmdutil.AddLogFlags(fs, "also write logs to this file (stdout carries the MCP protocol; logs go to stderr)")
	rankerName := cmdutil.AddRankerFlag(fs)
	if code, ok := cmdutil.ParseFlags(fs, args); !ok {
		return code
	}
	ranker, err := rank.Named(*rankerName)
	if err != nil {
		return cmdutil.Fail(stderr, err)
	}
	log, closeLog, err := logs.Open(stderr, "")
	if err != nil {
		return cmdutil.Fail(stderr, err)
	}
	defer closeLog()
	client, err := verifier.HTTPClient(*ca, 30*time.Second)
	if err != nil {
		return cmdutil.Fail(stderr, err)
	}
	client = httplog.Wrap(client, log)
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := sourcedmcp.NewServer(&sourcedmcp.Local{V: verifier.New(client, verifier.WithLogger(log)), Ranker: ranker}, "sourced-local", version, sourcedmcp.WithLogger(log))
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return cmdutil.Fail(stderr, err)
	}
	return 0
}
