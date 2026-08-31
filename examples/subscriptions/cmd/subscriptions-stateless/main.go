// Command subscriptions-stateless runs the tasks-mcp server in
// stateless Streamable HTTP mode — the deployment shape for
// horizontally scaled servers behind a plain round-robin load
// balancer, with no session affinity anywhere.
//
// Stateless mode is also the only mode in which the Go SDK speaks
// protocol revision 2026-07-28, which replaces `resources/subscribe`
// with `subscriptions/listen`: one long-lived POST whose response
// stream carries the notifications. That flips where subscription
// state lives — on the connection, not in a server-side session map:
//
//   - Any replica can serve any request; nothing routes on a session.
//   - A subscription lives exactly as long as its listen stream. The
//     replica holding the stream delivers `notifications/resources/
//     updated` for it. A dropped stream is NOT re-issued: on go-sdk
//     v1.7.0 these streams carry no SSE event IDs, the client abandons
//     a listen POST that dies without one, and — because the listen
//     call is dispatched fire-and-forget — no error surfaces to the
//     application. A later Subscribe for the same URI is a no-op while
//     the client still thinks it is subscribed. Each Subscribe opens
//     its own per-URI listen stream, so a separate heartbeat resource
//     attests only its own stream: to detect a dead stream, the server
//     must touch every watched URI on an interval — this binary does,
//     via watchHeartbeat (-heartbeat, default 30s). Recovery on go-sdk
//     v1.7.0 is a full reconnect: Unsubscribe (like any canceled
//     in-flight call) emits a notifications/cancelled POST missing the
//     _meta protocolVersion the 2026-07-28 protocol requires, the
//     server rejects it, and the client hard-fails the whole session —
//     so close the ClientSession, Connect a fresh one (any replica
//     answers), re-Subscribe every URI, and re-read to reconcile.
//   - SubscribeHandler / UnsubscribeHandler fire per URI exactly as in
//     the legacy modes — `subscriptions/listen` routes through the same
//     gate — so ACL checks carry over unchanged.
//   - Each replica pushes ResourceUpdated for events it observes. With
//     a shared event source (pub/sub, CDC, PG LISTEN) every replica
//     sees every event, so whichever replica holds a given listen
//     stream delivers to it.
//
// PropagateRequestCancellation ties every in-flight handler's context
// to its HTTP request: when the client goes away mid-call, the handler
// is canceled instead of running to completion for nobody. (For
// subscriptions/listen the SDK forces this on regardless — a listen
// handler blocks until its request ends, so it would otherwise never
// return.)
//
// Usage:
//
//	go run ./examples/subscriptions/cmd/subscriptions-stateless            # listens on 127.0.0.1:8080
//	go run ./examples/subscriptions/cmd/subscriptions-stateless -addr :9000
//	go run ./examples/subscriptions/cmd/subscriptions-stateless -heartbeat 5s  # faster liveness beats
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	tasksserver "github.com/akuity/protomcp/examples/tasks/server"
	tasksv1 "github.com/akuity/protomcp/pkg/api/gen/examples/tasks/v1"
	"github.com/akuity/protomcp/pkg/protomcp"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address for the MCP server")
	heartbeat := flag.Duration("heartbeat", 30*time.Second,
		"interval for touching every watched URI so clients can detect a dead listen stream (0 disables)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *addr, *heartbeat)
	stop()
	if err != nil {
		log.Fatalf("subscriptions-stateless: %v", err)
	}
}

// newStatelessServer builds the MCP server in the shape this example
// exists to demonstrate. Split out so the e2e tests exercise exactly
// what the binary runs; nil handlers default to allow-all. The SDK
// calls the subscribe/unsubscribe gate per URI on both the legacy
// resources/subscribe path and the 2026-07-28 subscriptions/listen
// path, so this is where an ACL would live.
func newStatelessServer(
	grpcClient tasksv1.TasksClient,
	onSubscribe func(context.Context, *mcp.SubscribeRequest) error,
	onUnsubscribe func(context.Context, *mcp.UnsubscribeRequest) error,
) *protomcp.Server {
	if onSubscribe == nil {
		onSubscribe = func(context.Context, *mcp.SubscribeRequest) error { return nil }
	}
	if onUnsubscribe == nil {
		onUnsubscribe = func(context.Context, *mcp.UnsubscribeRequest) error { return nil }
	}
	srv := protomcp.New("tasks-subscriptions-stateless-mcp", "0.1.0",
		protomcp.WithSDKOptions(&mcp.ServerOptions{
			SubscribeHandler:   onSubscribe,
			UnsubscribeHandler: onUnsubscribe,
		}),
		protomcp.WithHTTPOptions(&mcp.StreamableHTTPOptions{
			Stateless:                    true,
			PropagateRequestCancellation: true,
		}),
	)
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)
	tasksv1.RegisterTasksMCPResources(srv, grpcClient)
	return srv
}

func run(ctx context.Context, addr string, heartbeat time.Duration) error {
	// 1. Start the Tasks gRPC service. Its OnChange hook fires on every
	//    CRUD mutation and becomes our push point below. In a real
	//    multi-replica deployment this would be a shared event source
	//    (pub/sub, CDC, PG LISTEN) consumed by every replica.
	tSrv := tasksserver.New()
	grpcClient, shutdownGRPC, err := startTasksGRPC(ctx, tSrv)
	if err != nil {
		return fmt.Errorf("start grpc: %w", err)
	}
	defer shutdownGRPC()

	// 2. Build the stateless MCP server. The subscribe/unsubscribe gate
	//    feeds the heartbeat's refcount of watched URIs.
	hb := newWatchHeartbeat()
	srv := newStatelessServer(grpcClient,
		func(_ context.Context, req *mcp.SubscribeRequest) error {
			if err := rejectRequestScopedSubscribe(req); err != nil {
				return err
			}
			hb.subscribed(req.Params.URI)
			return nil
		},
		func(_ context.Context, req *mcp.UnsubscribeRequest) error {
			hb.unsubscribed(req.Params.URI)
			return nil
		},
	)
	if heartbeat > 0 {
		go hb.run(ctx, srv, heartbeat)
	}

	// 3. Push path: identical to the stateful examples. The SDK routes
	//    each ResourceUpdated to whichever live listen streams (or
	//    legacy sessions) subscribed to that URI on this replica.
	tSrv.OnChange = func(id string) {
		uri := "tasks://" + id
		if nErr := srv.SDK().ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: uri}); nErr != nil {
			log.Printf("ResourceUpdated %s: %v", uri, nErr)
		}
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Printf("tasks-subscriptions-stateless-mcp listening on %s (stateless, protocol >= 2026-07-28 capable)\n", addr)
	fmt.Println("  resources: tasks://{id} (read + list + push-on-mutation subscribe via subscriptions/listen)")
	fmt.Println("  tools:     Tasks_ListTasks, Tasks_GetTask, Tasks_CreateTask,")
	fmt.Println("             Tasks_UpdateTask, Tasks_DeleteTask")
	if heartbeat > 0 {
		fmt.Printf("  heartbeat: every watched URI touched every %s (missed beat = dead stream)\n", heartbeat)
	}

	errCh := make(chan error, 1)
	go func() {
		if sErr := httpSrv.ListenAndServe(); sErr != nil && !errors.Is(sErr, http.ErrServerClosed) {
			errCh <- sErr
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case sErr := <-errCh:
		return sErr
	}
}

// rejectRequestScopedSubscribe refuses a legacy resources/subscribe on this
// stateless server: the ephemeral per-POST session dies with the response, so
// the subscription could never deliver — and go-sdk tears such sessions down
// without firing UnsubscribeHandler (v1.7.0 server.go disconnect), which
// would leak watchHeartbeat's refcount. Subscriptions established via
// subscriptions/listen carry the SEP-2575 _meta protocol version and pass.
func rejectRequestScopedSubscribe(req *mcp.SubscribeRequest) error {
	if v, _ := req.Params.GetMeta()[mcp.MetaKeyProtocolVersion].(string); v >= "2026-07-28" {
		return nil
	}
	return fmt.Errorf("stateless server: a resources/subscribe subscription dies with its request; use subscriptions/listen (protocol >= 2026-07-28)")
}

// watchHeartbeat refcounts watched URIs via the subscribe/unsubscribe gate
// and touches each one on an interval, so a client can treat a missed
// heartbeat as its listen stream's death (see the README's liveness section).
type watchHeartbeat struct {
	mu      sync.Mutex
	watched map[string]int // URI -> subscriber count
}

func newWatchHeartbeat() *watchHeartbeat {
	return &watchHeartbeat{watched: map[string]int{}}
}

func (h *watchHeartbeat) subscribed(uri string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watched[uri]++
}

// count reports uri's current subscriber refcount.
func (h *watchHeartbeat) count(uri string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.watched[uri]
}

func (h *watchHeartbeat) unsubscribed(uri string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.watched[uri]--; h.watched[uri] <= 0 {
		delete(h.watched, uri)
	}
}

// run touches every watched URI once per interval until ctx ends.
func (h *watchHeartbeat) run(ctx context.Context, srv *protomcp.Server, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.mu.Lock()
			uris := make([]string, 0, len(h.watched))
			for uri := range h.watched {
				uris = append(uris, uri)
			}
			h.mu.Unlock()
			for _, uri := range uris {
				if err := srv.SDK().ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: uri}); err != nil {
					log.Printf("heartbeat %s: %v", uri, err)
				}
			}
		}
	}
}

func startTasksGRPC(ctx context.Context, impl tasksv1.TasksServer) (tasksv1.TasksClient, func(), error) {
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("listen: %w", err)
	}
	grpcSrv := grpc.NewServer()
	tasksv1.RegisterTasksServer(grpcSrv, impl)
	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		grpcSrv.Stop()
		_ = lis.Close()
		return nil, nil, fmt.Errorf("dial: %w", err)
	}

	cleanup := func() {
		_ = conn.Close()
		grpcSrv.GracefulStop()
	}
	return tasksv1.NewTasksClient(conn), cleanup, nil
}
