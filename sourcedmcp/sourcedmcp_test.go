package sourcedmcp_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sourcednet/core"
	"github.com/sourcednet/publisher"
	"github.com/sourcednet/resolver"
	"github.com/sourcednet/resolver/sourcedmcp"
	"github.com/sourcednet/resolver/verifier"
	"github.com/sourcednet/testkit/testnet"
	"github.com/sourcednet/testkit/testsite"
)

const (
	bridgeURL = "https://daily-herald.test/news/2026/09/bridge-reopens.html"
	bridgeRel = "news/2026/09/bridge-reopens.html"
)

var ctx = context.Background()

// connect starts an MCP session with the tools on b, over in-memory transports.
func connect(t *testing.T, b sourcedmcp.Backend, opts ...sourcedmcp.Option) *mcp.ClientSession {
	t.Helper()
	server := sourcedmcp.NewServer(b, "sourced-test", "test", opts...)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func citeOf(t *testing.T, text, containing string) string {
	t.Helper()
	parts := strings.Split(text, "Cite: ")
	for _, p := range parts[1:] {
		if strings.Contains(p, containing) {
			return strings.TrimSpace(strings.SplitN(p, "\n", 2)[0])
		}
	}
	t.Fatalf("no cite for a passage containing %q in:\n%s", containing, text)
	return ""
}

func backends(t *testing.T) map[string]func(n *testnet.Network) sourcedmcp.Backend {
	return map[string]func(n *testnet.Network) sourcedmcp.Backend{
		"local": func(n *testnet.Network) sourcedmcp.Backend {
			return &sourcedmcp.Local{V: verifier.New(n.Client())}
		},
		"resolver": func(n *testnet.Network) sourcedmcp.Backend {
			r, err := resolver.New(resolver.Config{Name: "resolver.test", DataDir: t.TempDir(), HTTP: n.Client(), Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close() })
			return r
		},
	}
}

// TestDemoFlow is the demo, as tool calls: read a page, cite a passage,
// the publisher corrects it, and resolving the citation shows the correction.
func TestDemoFlow(t *testing.T) {
	for name, mk := range backends(t) {
		t.Run(name, func(t *testing.T) {
			n := testnet.Standard(t)
			cs := connect(t, mk(n))

			tools, err := cs.ListTools(ctx, nil)
			if err != nil || len(tools.Tools) != 4 {
				t.Fatalf("tools: %v, %v", tools, err)
			}

			text, isErr := call(t, cs, "sourced_fetch", map[string]any{"url": bridgeURL, "query": "how much did the repairs cost", "max_chunks": 1})
			if isErr || !strings.Contains(text, "Signed by daily-herald.test") || !strings.Contains(text, "2.4 million") {
				t.Fatalf("fetch:\n%s", text)
			}
			cite := citeOf(t, text, "2.4 million")

			site := n.Site("daily-herald.test")
			site.WriteFile(bridgeRel, strings.Replace(site.Read(bridgeRel), "2.4 million", "3.1 million", 1))
			site.Build(testsite.T0.Add(time.Hour), map[string]publisher.Declaration{
				bridgeRel: {Change: core.ChangeCorrection, Note: "An earlier version gave the wrong cost."},
			})

			text, isErr = call(t, cs, "sourced_resolve", map[string]any{"citation": cite})
			if isErr || !strings.Contains(text, "State: CORRECTED") || !strings.Contains(text, "wrong cost") || !strings.Contains(text, "3.1 million") {
				t.Fatalf("resolve:\n%s", text)
			}

			text, isErr = call(t, cs, "sourced_verify", map[string]any{"url": bridgeURL, "passage": "The repairs cost 3.1 million"})
			if isErr || !strings.HasPrefix(text, "FOUND") {
				t.Fatalf("verify:\n%s", text)
			}
		})
	}
}

func TestNotParticipatingAndErrors(t *testing.T) {
	n := testnet.Standard(t)
	cs := connect(t, &sourcedmcp.Local{V: verifier.New(n.Client())})

	text, isErr := call(t, cs, "sourced_fetch", map[string]any{"url": "https://plain-site.test/"})
	if isErr || !strings.Contains(text, "not participating") {
		t.Fatalf("plain site:\n%s", text)
	}
	if text, isErr := call(t, cs, "sourced_verify", map[string]any{"passage": "anything at all here"}); !isErr || !strings.Contains(text, "needs a resolver") {
		t.Fatalf("local verify without url should be a tool error:\n%s", text)
	}
	if text, isErr := call(t, cs, "sourced_search", map[string]any{"query": "bridge repairs"}); !isErr || !strings.Contains(text, "needs a resolver") {
		t.Fatalf("local search should be a tool error:\n%s", text)
	}
}

// TestFindingPagesAndFragments covers two things a user hit in the demo: a
// site root that isn't a signed page, and a quote that is only part of a
// sentence.
func TestFindingPagesAndFragments(t *testing.T) {
	n := testnet.Standard(t)
	cs := connect(t, backends(t)["resolver"](n))

	text, isErr := call(t, cs, "sourced_fetch", map[string]any{"url": "https://longform.test/"})
	if isErr || !strings.Contains(text, "not one of its signed pages") || !strings.Contains(text, "https://longform.test/essays/keeping-things.html") {
		t.Fatalf("site root:\n%s", text)
	}

	// Syncing isn't needed: the fetch above doesn't index anything, so fetch the essay first.
	call(t, cs, "sourced_fetch", map[string]any{"url": "https://longform.test/essays/keeping-things.html"})
	text, isErr = call(t, cs, "sourced_verify", map[string]any{"passage": "the archive grew in ways its founders had not planned for"})
	if isErr || !strings.Contains(text, "exact match") || !strings.Contains(text, "longform.test/essays/keeping-things.html") {
		t.Fatalf("fragment lookup:\n%s", text)
	}

	text, _ = call(t, cs, "sourced_verify", map[string]any{"passage": "the archive grew"})
	if !strings.Contains(text, "too short") {
		t.Fatalf("short passage:\n%s", text)
	}

	// Without a URL, search finds the page among what the resolver holds.
	text, isErr = call(t, cs, "sourced_search", map[string]any{"query": "what did the archive keepers learn"})
	if isErr || !strings.Contains(text, "https://longform.test/essays/keeping-things.html") || !strings.Contains(text, "Cite: ") {
		t.Fatalf("search:\n%s", text)
	}
}

// TestNextPage pages through a page's passages, as the tool description
// tells the model to when the first passages don't answer.
func TestNextPage(t *testing.T) {
	for name, mk := range backends(t) {
		t.Run(name, func(t *testing.T) {
			cs := connect(t, mk(testnet.Standard(t)))
			args := map[string]any{"url": bridgeURL, "query": "bridge repairs", "max_chunks": 1}
			first, isErr := call(t, cs, "sourced_fetch", args)
			if isErr || !strings.Contains(first, "Passages 1–1") || !strings.Contains(first, "offset 1") {
				t.Fatalf("first page:\n%s", first)
			}
			args["offset"] = 1
			second, isErr := call(t, cs, "sourced_fetch", args)
			if isErr || !strings.Contains(second, "[2] ") || citeOf(t, second, "") == citeOf(t, first, "") {
				t.Fatalf("second page:\n%s", second)
			}
			args["offset"] = 99
			if past, _ := call(t, cs, "sourced_fetch", args); !strings.Contains(past, "No more passages after the first 99") {
				t.Fatalf("past the end:\n%s", past)
			}
		})
	}
}

// TestFetchAround reads a cited passage with its neighbors.
func TestFetchAround(t *testing.T) {
	for name, mk := range backends(t) {
		t.Run(name, func(t *testing.T) {
			cs := connect(t, mk(testnet.Standard(t)))
			text, isErr := call(t, cs, "sourced_fetch", map[string]any{"url": bridgeURL, "query": "repairs cost", "max_chunks": 1})
			if isErr {
				t.Fatalf("fetch:\n%s", text)
			}
			cite := citeOf(t, text, "2.4 million")
			text, isErr = call(t, cs, "sourced_fetch", map[string]any{"url": bridgeURL, "around": cite})
			if isErr || !strings.Contains(text, "asked about and those around it") || strings.Count(text, "Cite: ") < 2 || !strings.Contains(text, cite) {
				t.Fatalf("around:\n%s", text)
			}
		})
	}
}

// TestStructuredContentIsOptIn checks that models get only the text by
// default: the structured answer repeats every passage.
func TestStructuredContentIsOptIn(t *testing.T) {
	n := testnet.Standard(t)
	args := &mcp.CallToolParams{Name: "sourced_fetch", Arguments: map[string]any{"url": bridgeURL, "query": "repairs cost"}}
	for _, tt := range []struct {
		opts []sourcedmcp.Option
		want bool
	}{{nil, false}, {[]sourcedmcp.Option{sourcedmcp.WithStructuredContent()}, true}} {
		res, err := connect(t, &sourcedmcp.Local{V: verifier.New(n.Client())}, tt.opts...).CallTool(ctx, args)
		if err != nil || res.IsError {
			t.Fatalf("fetch: %v", err)
		}
		if got := res.StructuredContent != nil; got != tt.want {
			t.Errorf("structured content: got %v, want %v", got, tt.want)
		}
	}
}
