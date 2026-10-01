package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sourcednet/resolver/verifier"
)

// Client calls a resolver's HTTP API. It returns the same types as a
// verifier.Verifier, so code can use either.
type Client struct {
	// Base is the resolver's origin, e.g. https://resolver.example.net.
	Base string
	HTTP *http.Client
	// Originals asks the resolver to include the publishers' signed records
	// in its answers (see Resolver.AttachOriginals).
	Originals bool
}

// Fetch implements the verifier's Fetch through the resolver.
func (c *Client) Fetch(ctx context.Context, pageURL string) (*verifier.Page, error) {
	a, _, err := c.FetchAnswer(ctx, FetchRequest{URL: pageURL})
	if err != nil {
		return nil, err
	}
	return &a.Page, nil
}

// FetchAnswer fetches a page's passages as req asks, and returns the answer
// and its raw bytes.
func (c *Client) FetchAnswer(ctx context.Context, req FetchRequest) (*FetchAnswer, []byte, error) {
	q := url.Values{"url": {req.URL}}
	if req.Query != "" {
		q.Set("query", req.Query)
	}
	setCount(q, "max_chunks", req.MaxChunks)
	setCount(q, "offset", req.Offset)
	if req.Around != "" {
		q.Set("around", req.Around)
	}
	setCount(q, "context", req.Context)
	c.askOriginals(q)
	var a FetchAnswer
	raw, err := c.do(ctx, http.MethodGet, "fetch?"+q.Encode(), nil, &a)
	return &a, raw, err
}

// Resolve implements the verifier's Resolve through the resolver.
func (c *Client) Resolve(ctx context.Context, citation string) (*verifier.Resolution, error) {
	a, _, err := c.ResolveAnswer(ctx, citation)
	if err != nil {
		return nil, err
	}
	return &a.Resolution, nil
}

// ResolveAnswer resolves a citation and returns the answer and its raw bytes.
func (c *Client) ResolveAnswer(ctx context.Context, citation string) (*ResolveAnswer, []byte, error) {
	q := url.Values{"cite": {citation}}
	c.askOriginals(q)
	var a ResolveAnswer
	raw, err := c.do(ctx, http.MethodGet, "resolve?"+q.Encode(), nil, &a)
	return &a, raw, err
}

// VerifyPassage implements the verifier's VerifyPassage through the resolver.
func (c *Client) VerifyPassage(ctx context.Context, pageURL, passage string) (*verifier.PassageMatch, error) {
	var a VerifyAnswer
	if _, err := c.do(ctx, http.MethodPost, "verify", verifyRequest{Passage: passage, URL: pageURL, Originals: c.Originals}, &a); err != nil {
		return nil, err
	}
	return &a.PassageMatch, nil
}

// Lookup asks which stored sources contain a passage, without a URL.
func (c *Client) Lookup(ctx context.Context, passage string) (*LookupAnswer, error) {
	var a LookupAnswer
	_, err := c.do(ctx, http.MethodPost, "verify", verifyRequest{Passage: passage}, &a)
	return &a, err
}

// Search finds pages the resolver holds that match a query.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchAnswer, error) {
	q := url.Values{"q": {req.Query}}
	if req.Publisher != "" {
		q.Set("publisher", req.Publisher)
	}
	setCount(q, "max_results", req.MaxResults)
	setCount(q, "offset", req.Offset)
	c.askOriginals(q)
	var a SearchAnswer
	_, err := c.do(ctx, http.MethodGet, "search?"+q.Encode(), nil, &a)
	return &a, err
}

// Changes returns the change feed after since, optionally for one publisher.
func (c *Client) Changes(ctx context.Context, since int64, publisher string) (*ChangesAnswer, error) {
	q := url.Values{"since": {strconv.FormatInt(since, 10)}}
	if publisher != "" {
		q.Set("publisher", publisher)
	}
	var a ChangesAnswer
	_, err := c.do(ctx, http.MethodGet, "changes?"+q.Encode(), nil, &a)
	return &a, err
}

// Announce tells the resolver that domain has published.
func (c *Client) Announce(ctx context.Context, domain string) error {
	_, err := c.do(ctx, http.MethodPost, "announce", announceRequest{Domain: domain}, nil)
	return err
}

// setCount sets a numeric query parameter, unless it is 0.
func setCount(q url.Values, name string, n int) {
	if n != 0 {
		q.Set(name, strconv.Itoa(n))
	}
}

func (c *Client) askOriginals(q url.Values) {
	if c.Originals {
		q.Set("originals", "1")
	}
}

// APIError is a non-success answer from the resolver.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("resolver: %d %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, body, out any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Base, "/")+APIPrefix+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		return raw, &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("resolver answer: %w", err)
		}
	}
	return raw, nil
}
