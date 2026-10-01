# resolver

`sourced-resolver` runs a sourced.net resolver: it syncs publishers, verifies and keeps what they signed, and answers AI apps over HTTP and MCP (`sourced_search`, `sourced_fetch`, `sourced_resolve`, `sourced_verify`), signing every answer. It also runs the local validating client (`mcp`), which offers the same tools but checks everything itself, and a read-only view of a resolver's store (`inspect`).

Spec: `../../Docs/sourced.net — Core Spec v1.md`. Go 1.24 or later.

```
make            # lint, test, build: bin/sourced-resolver
```

## Running a resolver

```
sourced-resolver serve -name resolver.example.net -publisher example.org -data ./resolver-data
```

- **Plain HTTP** on `-addr` (default `:8080`): put TLS in front (nginx, Caddy).
- **MCP** at `/mcp`: add it to Claude Code with `claude mcp add --transport http <name> https://resolver.example.net/mcp`.
- **`-ranker`** orders passages for queries and search: `bm25` (default) or `bm25-lead`, which favors a page's opening.
- **`-index`** picks the passage index: `windows` (default) or `winnowed`, smaller but slightly worse at short quotes. Changing it rebuilds the index in the background.
- **`-freshness`** is how long a page's record is trusted before rechecking (default 5 minutes); `resolve` always checks live.
- **`-log <file>` and `-v`** for logs (below).

Every answer is signed, and can be checked later using only public files (`sourced-publisher check answer.json`). Answers name the publishers' records they rely on; add `originals=1` (or `"originals": true` to a `verify` body) to include the signed records themselves.

| Endpoint | Does |
| --- | --- |
| `GET /sourced/v1/fetch?url=&query=&max_chunks=&offset=` | A page's verified chunks, ranked by the query; `next_offset` in the answer gets the next page. `around=<cite>&context=1` gives a passage with its neighbors instead |
| `GET /sourced/v1/search?q=&publisher=&max_results=&offset=` | Pages the resolver holds that match a query, each with its best passage, paged the same way |
| `GET /sourced/v1/resolve?cite=` | A citation's current state, checked live |
| `POST /sourced/v1/verify` `{"passage", "url"?}` | Whether a passage is in a page, or who published it |
| `GET /sourced/v1/changes?since=&publisher=` | The change feed |
| `POST /sourced/v1/announce` `{"domain"}` | A publisher's hint to sync now |
| `GET /sourced/v1/objects/<hex>` | A record, chunk, or bundle (`?bundle`) by hash |
| `GET /.well-known/sourced/resolver.json` | The resolver's keys and details |

**Without a resolver,** `sourced-resolver mcp` is the validating client over stdio: it fetches from publishers directly and checks every signature itself (no search, and no passage lookup without a URL, since those need a resolver's index).

## Seeing what happens

**Logs:** one line per event.
- `tool`: an MCP tool call, with its arguments, outcome, and duration.
- `sync`: a publisher sync, `not modified` or `updated`, with pages and changes.
- `change` and `purge`: what changed.
- `page`: how a page was answered: `fresh` from the store, `live` from the publisher, or `stale` when the publisher was unreachable.
- `resolve` and `lookup`: resolves, and passage lookups with how many word windows matched.
- `index`: new text added to the passage index, shortly after the request that brought it in.

`-v` adds every key, record, and bundle (and whether it came from the cache or the network) and every HTTP request.

**Data:** `sourced-resolver inspect` is read-only, so it's safe while a resolver runs.

```
sourced-resolver inspect -data ./resolver-data                       # publishers, sync state, counts, disk use (a demo folder works too)
sourced-resolver inspect -data ./resolver-data pages example.org
sourced-resolver inspect -data ./resolver-data record ea4b6f7a       # by ID prefix (as in the logs) or page URL
sourced-resolver inspect -data ./resolver-data record ea4b6f7a -raw  # the signed JSON as stored
sourced-resolver inspect -data ./resolver-data chunk c75bae89
sourced-resolver inspect -data ./resolver-data changes
sourced-resolver inspect -data ./resolver-data search "a quote of six words or more"
```

The data directory holds `resolver.db` (SQLite: publishers, records, chunks, which record is current per page, the change feed, keys, the passage index) and `chunks/`, one Markdown file per chunk, named by its hash.

## Packages

- `resolver` (this directory): the resolver, its store, sync, signed answers, and an HTTP client for it.
- `verifier`: the spec's procedures (fetch, resolve, passage checks), shared by the resolver and the validating client.
- `rank`, `passage`: the swappable ranker and passage index.
- `sourcedmcp`: the MCP tools, on a resolver or the validating client.
- `cmdutil`, `httplog`: shared by the command line tools.

## Parts

A resolver syncs publishers, keeps what it has verified, and answers apps. These are its parts, where each lives, and what can be swapped. Implementations are compared with `sourced bench compare`.

| Part | Does | Lives in | Swappable today |
| --- | --- | --- | --- |
| **Sources** | Fetch and verify keys, manifests, records, and bundles from publishers | `verifier.Verifier`, wired in through its `Observer` and `Cache` hooks (`storeHooks`) | No: publishers are the only source. Phase 13 adds upstream resolvers. |
| **Content store** | Signed records, chunk text, which record is current per page, publishers' sync state, the change feed | `ContentStore`, in five parts (`Objects`, `PagePointers`, `PublisherStates`, `ChangeFeed`, `Passages`); `Store` (SQLite plus content-addressed chunk files) is the default | **Yes:** `Config.Store` |
| **Passage index** | Which stored chunks hold a quoted passage | `passage.Index` decides what is stored; `Store` keeps it (`passage_windows`, indexed in the background) | **Yes:** `windows` (default) or `winnowed` |
| **Ranker** | Orders a page's passages for a query, and pages for search | `rank.Ranker` | **Yes:** `bm25` (default) or `bm25-lead`; `rank.WithLeadPrior` wraps any ranker |
| **Sync policy** | When to check which publisher: polling by `max-age`, announces, the freshness window | `SyncPolicy`; `Polling` is the default, built from `Config`'s poll and freshness settings | **Yes:** `Config.Sync` |
| **Answer surfaces** | Signed answers over HTTP and MCP | `server.go` (HTTP and signing), `sourcedmcp` (MCP) | MCP already runs on any `sourcedmcp.Backend`: this resolver or the local validating client |
| **Trust policy** | Which sources count for how much | Nothing yet | Future Versions: credibility levels, in Phase 13 |

Choose implementations in code (`Config.Ranker`, `Config.Index`, `Config.Store`, `Config.Sync`), on the command line (`sourced-resolver serve -ranker … -index …`, also `sourced-lab demo`), or in bench configs (`ranker:`, `index:`).

### Boundaries, and what an interface for each would cover

- **Sources.** A source must give records and bundles by ID, a publisher's current manifest, and its keys, each re-verified by the resolver. `Client` already speaks the resolver's HTTP API, so an upstream resolver needs mostly an adapter, plus the merging and provenance in Phase 13.
- **Content store.** `ContentStore` is five interfaces, so a store for another scale can replace them separately: `Objects` (records, chunk text, bundles, purges), `PagePointers` (which record is current per page), `PublisherStates` (sync state and keys), `ChangeFeed`, and `Passages` (lookups and background indexing). Measurements that only make sense for SQLite (`Stats`, `Checkpoint`) stay on `Store`; the bench opens a `Store` itself and passes it in.
- **Passage index.** `passage.Index` only decides which window hashes to store; storing and looking them up stays in `Store`. A lookup hashes every window of the quote and scores candidates on their full text, so any index that stores a subset of windows works. Switching index rebuilds it in the background.
- **Ranker.** `Scores(query, passages)` over a page's passages (section path and text). Ordering, grouping by page, and paging are shared (`rank.Order`, `rank.Groups`, `rank.Slice`). A semantic ranker would implement the same method; it needs a model, so it waits for a decision.
- **Sync policy.** `SyncPolicy` decides when to sync a publisher next (`NextSync`), whether an announce starts a sync (`AcceptAnnounce`), and how long a page's record is trusted (`Fresh`). `Sync` itself (fetch what's new, verify, store, record changes) is the same under any policy. Push subscriptions (v2), priorities, and budgets would be further policies.
