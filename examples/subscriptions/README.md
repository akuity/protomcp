# examples/subscriptions

Two runnable demonstrations of **user-wired resource subscriptions** on
top of protomcp-generated resources. protomcp does not codegen
subscribe handlers; the MCP Go SDK already provides everything you
need.

## Why protomcp doesn't codegen subscribe

MCP's `resources/subscribe` is a per-URI fanout: the server delivers
each `resources/updated` notification to every session subscribed to
that URI. gRPC server-streaming is the opposite shape, a per-request
stream owned by one caller. The two models do not map cleanly, and
real backends also deliver change events from places that are not
gRPC streams at all (Postgres `LISTEN/NOTIFY`, Redis/NATS pub/sub,
Kafka, CDC feeds, polling, webhooks). A codegen'd annotation would
pick a wrong default for most users.

The MCP Go SDK handles subscription mechanics itself. Supply
`SubscribeHandler` and `UnsubscribeHandler` on `mcp.ServerOptions`
(both are required; the SDK panics at `NewServer` if only one is
set), then call `srv.SDK().ResourceUpdated(ctx, params)` whenever a
resource changes. The SDK tracks per-session per-URI subscriptions
internally and fans each `ResourceUpdated` call out to only the
sessions that asked for that URI.

**The subscribe/unsubscribe handlers only act as a gate.** When a
subscription request arrives, the SDK calls your handler first
(`return err` to reject, `return nil` to allow); on allow, it
**unconditionally** records the session in its internal
subscriptions map. `ResourceUpdated` always reads from that same map,
so it fans out to subscribed sessions regardless of whether your
handlers are no-ops or doing real upstream work. The handler type
decides **which URIs are accepted**, not whether fan-out happens.

**How subscriptions arrive depends on the protocol revision.** Legacy
sessions (negotiated through `initialize`, up to 2025-11-25) send
`resources/subscribe` / `resources/unsubscribe` requests. Sessions on
revision 2026-07-28 or later hold a long-lived `subscriptions/listen`
stream instead, and `resources/subscribe` is rejected there — but the
SDK routes each listed URI through the same internal subscribe path,
so your `SubscribeHandler`/`UnsubscribeHandler` fire per URI on both
paths and `ResourceUpdated` delivers over whichever channel the
session holds. The go-sdk client picks the mechanism transparently.

## Pick the pattern that matches your event source

### Pattern A: push from the write path (simpler, more common)

Supply **no-op** `SubscribeHandler` and `UnsubscribeHandler`, then
call `ResourceUpdated` directly from wherever mutations happen. The
handlers only need to return `nil` (returning an error rejects the
subscribe). This is the pattern the MCP Go SDK's own conformance
server uses.

```go
srv := protomcp.New("svc", "0.1.0",
    protomcp.WithSDKOptions(&mcp.ServerOptions{
        SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
        UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
    }),
)

// From any code path that mutates a resource:
srv.SDK().ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{
    URI: "tasks://" + id,
})
```

Use this when mutation events originate inside the same process as
the MCP server: a gRPC write handler, an HTTP mutation endpoint, a
background job updating shared state. No indirection, no extra
moving parts.

**Runnable demo:** [`cmd/subscriptions-simple`](cmd/subscriptions-simple). The
Tasks gRPC service's `OnChange` hook directly calls `ResourceUpdated`.
About 30 lines of wiring.

### Pattern B: wrap an external source

Use a real `SubscribeHandler` when you need to start upstream
delivery per subscription: open a gRPC stream, run a PG `LISTEN`,
subscribe to a Redis or Kafka topic, register a webhook. Your
handler does the "start feed for this URI" work; your
`UnsubscribeHandler` stops it. When events arrive from the external
source, you still call `ResourceUpdated` to fan them out: the SDK's
session-tracking behaviour is the same as Pattern A, the custom
handlers only add gating (e.g. reject unknown URIs) and lifecycle
hooks for starting / stopping upstream work.

```go
var srv *protomcp.Server
var once sync.Once

watch := func() {
    go protomcp.RetryLoop(ctx, func(ctx context.Context, reset func()) error {
        // In production this opens a server-streaming gRPC Watch RPC,
        // a PG LISTEN, a Kafka consumer, etc. On successful open,
        // call reset() so a later failure starts RetryLoop's backoff
        // from zero.
        stream, err := openWatch(ctx)
        if err != nil { return err }
        reset()
        for {
            evt, err := stream.Recv()
            if err != nil { return err }
            srv.SDK().ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{
                URI: evt.URI,
            })
        }
    })
}

subscribe := func(ctx context.Context, req *mcp.SubscribeRequest) error {
    // ACL check first (see next section).
    if err := authorize(ctx, req); err != nil { return err }
    once.Do(watch) // lazy start; no watcher until the first subscribe
    return nil
}

srv = protomcp.New("svc", "0.1.0",
    protomcp.WithSDKOptions(&mcp.ServerOptions{
        SubscribeHandler:   subscribe,
        UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
    }),
)
```

Why this shape:

- **One global watcher, not one-per-subscribe.** The MCP Go SDK
  handles per-session fan-out itself; `srv.SDK().ResourceUpdated`
  only needs to be called once per event and the SDK delivers to
  every interested session. Opening a new upstream watch per
  subscriber would usually waste backend resources.
- **Lazy via `sync.Once`.** Before the first subscribe, the SDK's
  subscription map is empty and any fan-out is a no-op, so there is
  nothing to do. Starting the watcher on first subscribe avoids
  consuming upstream capacity for an idle server.
- **`RetryLoop` for resilience.** Upstream watch streams drop
  (network blip, server restart, token expiry). `RetryLoop`
  reconnects with exponential backoff (100 ms to 30 s, ±10% jitter);
  call `reset()` after a successful open so transient failures do
  not accumulate backoff.
- **Unsubscribe is a no-op.** The SDK removes the session from its
  map automatically. The watcher keeps running for other subscribers;
  when the map is empty `ResourceUpdated` is a cheap no-op.

### Authz on subscribe

Because `SubscribeHandler` sees the full request ctx, it is the
natural place to enforce per-principal ACLs on which URIs a caller
may subscribe to. The demo does it as a composable wrapper:

```go
func authorize(next subscribeHandler) subscribeHandler {
    return func(ctx context.Context, req *mcp.SubscribeRequest) error {
        p := principalFromContext(ctx)
        if p == nil { return errors.New("unauthenticated") }
        for _, prefix := range p.AllowedURIPrefixes {
            if strings.HasPrefix(req.Params.URI, prefix) {
                return next(ctx, req)
            }
        }
        return fmt.Errorf("%q not permitted to subscribe to %q", p.UserID, req.Params.URI)
    }
}
```

The principal is stashed on ctx by an HTTP middleware wrapping the
MCP server (`authMiddleware` in [`authz.go`](cmd/subscriptions/authz.go)).
For production auth, swap the demo bearer table for the MCP Go SDK's
[`auth.RequireBearerToken`](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/auth#RequireBearerToken)
or your IdP of choice. `examples/auth/cmd/sdkauth` shows that
wiring.

**Unsubscribe is not re-checked.** A caller can always release a
subscription they previously held; denying unsubscribe when access
has been revoked just leaks server state.

**Runnable demo:** [`cmd/subscriptions`](cmd/subscriptions). The
Tasks gRPC service's `OnChange` hook stands in for the Watch stream;
[`authz.go`](cmd/subscriptions/authz.go) provides the bearer
middleware and ACL wrapper. Two demo tokens:

| Token | May subscribe to |
|---|---|
| `alice-token` | any `tasks://*` |
| `bob-token` | only `tasks://1` |

### Combo

Dispatch inside `SubscribeHandler` on URI pattern or scheme when some
resources push from internal code and others need an external watch.
Return `nil` for push-path URIs (no-op) and open the external watch
only for URIs that need it.

## Serving stateless (protocol ≥ 2026-07-28, no session affinity)

`cmd/subscriptions-stateless` runs the same subscription wiring with

```go
protomcp.WithHTTPOptions(&mcp.StreamableHTTPOptions{
    Stateless:                    true,
    PropagateRequestCancellation: true,
})
```

Stateless mode is the shape for horizontally scaled servers behind a
plain round-robin load balancer — and it is the only mode in which the
Go SDK speaks protocol revision 2026-07-28, where `resources/subscribe`
is replaced by `subscriptions/listen`: one long-lived POST whose
response stream carries the notifications. Subscription state lives on
that connection, not in a server-side session map, so no affinity
mechanism is needed: any replica can serve any request, and the replica
holding a listen stream delivers to it. Your `SubscribeHandler` /
`UnsubscribeHandler` fire per URI exactly as in the other patterns —
`subscriptions/listen` routes through the same gate — and the push
side (`ResourceUpdated`) is unchanged; in a multi-replica deployment,
feed every replica from a shared event source so whichever one holds a
given stream can deliver. One asymmetry to guard: a legacy
`resources/subscribe` also reaches the gate on a stateless server, but
its per-POST session dies with the response — the subscription could
never deliver, and go-sdk tears the session down without firing
`UnsubscribeHandler`, so any gate-side bookkeeping (the demo's
heartbeat refcount included) leaks +1 with no matching -1. The demo
rejects those legacy lifecycle RPCs outright
(`requireListenScopedSubscription`): only `subscriptions/listen`
subscriptions, whose params carry the SEP-2575 `_meta` protocol version,
are accepted.

**A dropped listen stream is not replaced, and the loss is silent.** On
go-sdk v1.7.0, streams on this protocol carry no SSE event IDs, and the
client abandons a POST stream whose connection dies without one instead
of retrying it; because `subscriptions/listen` is dispatched
fire-and-forget, the synthesized `request terminated without response`
error is discarded, so no error surfaces to the application. A later
`ClientSession.Subscribe` for the same URI is a no-op while the client
still believes it is subscribed (and a `Subscribe` whose initial listen
call fails leaves the same stale entry behind), so a dead subscription
cannot be repaired in place by subscribing again.

Detection has to ride each watched URI's own stream:
`ClientSession.Subscribe` opens a separate `subscriptions/listen` POST
per URI, so a dedicated heartbeat resource attests only its own stream
and can keep beating while the stream for the URI you actually watch
is already dead. To notice a dead stream, have the server emit a
periodic update for every watched URI and treat a missed heartbeat as
that stream's death — at the cost of one update (and, since a
heartbeat is indistinguishable from a real change, one re-read) per
URI per interval. The stateless demo implements this: `watchHeartbeat`
refcounts watched URIs from the subscribe/unsubscribe gate and touches
each one on the `-heartbeat` interval (default 30s, `0` disables). Periodic re-reads of the watched resource remain a
reconciliation fallback, not a liveness check: each read is its own
stateless POST and succeeds whether or not the listen stream is alive,
so polling bounds how stale a client can silently become, but an
unchanged resource reveals nothing and a dead stream goes undetected.

Recovery is session-scoped on go-sdk v1.7.0. The natural per-URI cycle
— `Unsubscribe` to clear the client-side entry, then a fresh
`Subscribe` — does not survive contact with the implementation:
canceling the listen call makes the client send a
`notifications/cancelled` POST without the
`_meta["io.modelcontextprotocol/protocolVersion"]` the 2026-07-28
protocol requires on every message (`cancelCall` skips the
`injectRequestMeta` step every other client send performs), the server
rejects it (`-32602` over HTTP 400), and the client treats the failed
send as fatal and permanently fails the connection. From that point
every awaited call returns `connection closed` and every
fire-and-forget send — a re-`Subscribe` included — vanishes silently;
canceling any in-flight request poisons the session through the same
path. Until that is fixed upstream, recover by replacing the session:
close the poisoned `ClientSession`, `Connect` a fresh one (any replica
answers), re-`Subscribe` every watched URI, and re-read each one to
reconcile updates missed while dark. The e2e suite pins both halves —
the poisoning and the reconnect recovery.

`PropagateRequestCancellation` ties each in-flight handler context to
its HTTP request, so a client that goes away mid-call cancels the
handler instead of leaving it running for nobody. (The SDK forces this
on for `subscriptions/listen` requests regardless — a listen handler
blocks until its request ends.)

The e2e suite in `cmd/subscriptions-stateless/main_test.go` pins the
whole contract: 2026-07-28 negotiated over plain HTTP, listen-based
delivery to concurrent clients, unsubscribe teardown, heartbeats
arriving with no mutation at all, the Unsubscribe poisoning and its
reconnect recovery, handler cancellation on request abort, and the
same endpoint still answering a classic `initialize` from pre-2026
clients.

## Running the demos

```shell
# Pattern A: minimal push-from-write-path, no auth
go run ./examples/subscriptions/cmd/subscriptions-simple -addr :8080

# Pattern B: watch stream + authz (requires a bearer token)
go run ./examples/subscriptions/cmd/subscriptions -addr :8080

# Stateless: same push wiring, Stateless + PropagateRequestCancellation,
# subscriptions arrive via subscriptions/listen (protocol >= 2026-07-28);
# -heartbeat touches every watched URI on an interval for stream liveness
go run ./examples/subscriptions/cmd/subscriptions-stateless -addr :8080 -heartbeat 30s
```

Point any MCP client at `http://localhost:8080`. For Pattern B,
include `Authorization: Bearer alice-token` on every request. Call
`resources/subscribe` with `{URI: "tasks://<id>"}`, then mutate the
task via `Tasks_UpdateTask`. A `resources/updated` notification
arrives on every connected session that subscribed to that URI.

`bob-token` is rejected for `tasks://` subscriptions unless the URI
is exactly `tasks://1`; useful for exercising the authz path.
