package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/sourcednet/resolver/cmdutil"
	"io"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/sourcednet/resolver"
	"github.com/sourcednet/resolver/httplog"
	"github.com/sourcednet/resolver/passage"
	"github.com/sourcednet/resolver/rank"
	"github.com/sourcednet/resolver/verifier"
)

func cmdServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := cmdutil.NewFlags("serve", "", stderr)
	name := fs.String("name", "", "the resolver's public domain, e.g. resolver.example.net (required)")
	addr := fs.String("addr", ":8080", "address to listen on (plain HTTP; put TLS in front, e.g. Caddy)")
	data := fs.String("data", "resolver-data", "data directory: database, chunk files, and the resolver's key")
	var pubs cmdutil.ListFlag
	fs.Var(&pubs, "publisher", "publisher domain to sync from the start (repeatable)")
	freshness := fs.Duration("freshness", 5*time.Minute, "how long a page's current record is trusted before rechecking; 0 always rechecks")
	poll := fs.Duration("poll", 5*time.Minute, "manifest poll interval for publishers that send no max-age")
	ca := fs.String("ca", "", "dev only: also trust this PEM certificate authority when reaching publishers")
	allowPrivate := fs.Bool("allow-private", false, "dev only: allow publishers on private and loopback addresses")
	operator := fs.String("operator", "", "operator name published in resolver.json")
	logs := cmdutil.AddLogFlags(fs, "also write logs to this file")
	retention := fs.String("retention", "", "URL of the retention policy published in resolver.json")
	rankerName, indexName := cmdutil.AddRankerFlag(fs), cmdutil.AddIndexFlag(fs)
	if code, ok := cmdutil.ParseFlags(fs, args); !ok {
		return code
	}
	if *name == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	ranker, err := rank.Named(*rankerName)
	if err != nil {
		return cmdutil.Fail(stderr, err)
	}
	index, err := passage.Named(*indexName)
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
	if !*allowPrivate {
		if err := resolver.PublicOnly(client); err != nil {
			return cmdutil.Fail(stderr, err)
		}
	}
	client = httplog.Wrap(client, log)
	r, err := resolver.New(resolver.Config{
		Name: *name, DataDir: *data, HTTP: client,
		Freshness: *freshness, PollInterval: *poll,
		Operator: *operator, RetentionPolicy: *retention, Logger: log, MCPPath: cmdutil.MCPPath, Ranker: ranker, Index: index,
	})
	if err != nil {
		return cmdutil.Fail(stderr, err)
	}
	defer r.Close()
	for _, p := range pubs {
		if err := r.AddPublisher(ctx, p); err != nil {
			return cmdutil.Fail(stderr, err)
		}
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go r.Run(ctx, time.Second)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return cmdutil.Fail(stderr, err)
	}
	srv := &http.Server{Handler: cmdutil.Handler(r, version, log), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(stdout, "Resolver %s listening on %s (data in %s); MCP at %s\n", *name, ln.Addr(), *data, cmdutil.MCPPath)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return cmdutil.Fail(stderr, err)
	}
	return 0
}
