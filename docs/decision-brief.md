# attentiond MVP decision brief

Written before implementation, from the Herdr API schema shipped in
`herdrdev/herdr` at tag `v0.9.0` (`docs/next/api/herdr-api.schema.json`, protocol
22, schema version 1) and the Glance `custom-api` widget docs.

## Herdr integration approach

Herdr exposes a newline-delimited JSON socket API. Requests are
`{"id","method","params"}`; replies are `{"id","result"}` or `{"id","error":{"code","message"}}`.
The socket lives at `$XDG_CONFIG_HOME/herdr/herdr.sock`, with `HERDR_SOCKET_PATH`
and `HERDR_SESSION` overrides.

`attentiond` speaks that socket directly rather than shelling out to the `herdr`
CLI. The CLI is a wrapper over the same methods, so a subprocess per poll would
add fork cost and argument-quoting bugs for nothing.

It polls `session.snapshot` every two seconds. The response carries
`workspaces`, `tabs`, `panes`, `layouts` and `agents` in one request, which is
exactly the join `attentiond` needs. `events.subscribe` would give lower latency
and no redundant reads, but it needs a reconnect strategy, a buffered bootstrap
sequence (subscribe, snapshot, replay), and per-event merge logic. Two-second
polling of a local Unix socket is a rounding error next to that complexity. The
adapter is written so a subscription can replace the ticker later without
touching the store or the HTTP layer.

Only `snapshot.agents` becomes items. Herdr already classifies which pane
occupants are coding agents; plain shells are not work that needs attention.
No terminal output is read: `agent_status` is a semantic field, and
`pane.read` is never called.

## State mapping

Herdr `agent_status` is one of `idle`, `working`, `blocked`, `done`, `unknown`.

| Herdr | attentiond state | severity | why |
| --- | --- | --- | --- |
| `working` | `working` | `info` | agent is mid-turn |
| `idle` | `waiting` | `info` | ready for input, already seen |
| `blocked` | `needs_attention` | `warning` | Herdr matched an approval or question UI |
| `done` | `done` | `info` | finished and not yet seen |
| `unknown` | `waiting` | `info` | agent present, classification not confident |

Herdr has no failure signal, so `failed` only ever arrives from
`POST /api/events`.

The attention queue is states `needs_attention`, `failed` and `done`. Including
`done` is deliberate: Herdr reports `done` only while a finished agent is
*unseen*, and flips it to `idle` once the pane is focused. Focusing from Glance
therefore clears the item on the next poll, which is the loop the MVP is meant
to prove.

Generic lifecycle events map `started`/`working` to `working`, `waiting` to
`waiting`, `needs_attention` to `needs_attention`, `completed` to `done`, and
`failed` to `failed`. Event items in a terminal state expire after a TTL
(default one hour) so a long-lived daemon does not accumulate finished builds.
Adapter-owned items never expire; the adapter owns their whole lifecycle.

## Proposed API

| Route | Purpose |
| --- | --- |
| `GET /api/work` | every known item |
| `GET /api/attention` | items in `needs_attention`, `failed` or `done` |
| `GET /health` | daemon liveness plus per-source adapter health |
| `POST /api/events` | generic lifecycle ingestion |
| `POST /api/actions/{source}/{kind}/{target}/{action}` | run a local action |

Both list endpoints return the same envelope: `generated_at`, `count`,
`attention_count`, `items`. Each item carries a precomputed `attention` boolean
so a Glance template can branch without restating the state rules.

Items sort by severity, then by recency. A Glance widget that renders the first
N rows shows the most urgent work without client-side sorting.

## Action mechanism

Actions are data, not a plugin system. Each item carries
`actions: [{id, label, method, href}]` with an absolute URL, so a consumer posts
the href without knowing the route grammar. The route is generic
(`{source}/{kind}/{target}/{action}`); the daemon looks the source up in a small
registry of executors and hands over the remaining three segments.

The Herdr executor implements `focus`. For a pane it calls `pane.get` to recover
the owning workspace and tab, then `workspace.focus`, `tab.focus`, and
`agent.focus` when the pane hosts an agent. Herdr has no `pane.focus` by id
(`pane.focus_direction` is directional only), so `agent.focus` with the pane id
as target is the supported route to a specific pane.

One interface, one method:

```go
type Executor interface {
    Execute(ctx context.Context, kind, target, action string) error
}
```

That is the whole abstraction. A future GitHub or `tofu` adapter registers under
its own source name and gets the same route for free.

## Package structure

```
cmd/attentiond        flags, logger, wiring, graceful shutdown
internal/attention    Item, State, Severity, Action, in-memory Store
internal/herdr        socket client, snapshot normalization, poller, focus executor
internal/httpapi      routes, event ingestion, action dispatch
```

`attention` imports nothing local. `herdr` and `httpapi` both depend on
`attention` and not on each other; `httpapi` declares the `Executor` interface it
consumes, and `herdr` satisfies it without importing the HTTP package.

Dependencies: the standard library only. Go 1.22 `net/http` routing patterns
cover the routes, `log/slog` covers structured logging, and `encoding/json`
covers both wire formats.

## Persistence decision

In-memory only. Herdr-sourced items are rebuilt from `session.snapshot` within
one poll of a restart, so persisting them would only risk showing state that no
longer exists. Event items are genuinely lost on restart, which is the one real
cost. That is acceptable while the producers are foreground builds and tests
that a human is watching. If OMP hooks or long `tofu` runs start to lose
meaningful state across restarts, the fix is a single append-only JSON file
replayed at startup, not a database.
