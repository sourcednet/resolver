// Command sourced-resolver runs a sourced.net resolver (sync publishers, answer fetch,
// search, resolve, and verify over HTTP and MCP), the local validating
// client, and a read-only view of a resolver's store.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

const version = "0.1.0-dev"

const usage = `sourced-resolver — a sourced.net resolver (spec sourced/1)

Usage:
  sourced-resolver <command> [flags] [arguments]

Commands:
  serve     run a resolver: sync publishers and answer apps over HTTP and MCP
  mcp       run the validating client: the sourced MCP tools over stdio, checked locally
  inspect   show what a resolver has stored (read-only)
  version   print the version

Run "sourced-resolver <command> -h" for a command's flags.
`

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmds := map[string]func(context.Context, []string, io.Writer, io.Writer) int{
		"serve":   cmdServe,
		"mcp":     cmdMCP,
		"inspect": cmdInspect,
	}
	switch name := args[0]; name {
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		cmd, ok := cmds[name]
		if !ok {
			fmt.Fprintf(stderr, "sourced-resolver: unknown command %q\n\n%s", name, usage)
			return 2
		}
		return cmd(ctx, args[1:], stdout, stderr)
	}
}
