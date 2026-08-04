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
//     updated` for it; when the stream drops, the client re-issues it
//     (to whichever replica the balancer picks next).
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
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *addr)
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

func run(ctx context.Context, addr string) error {
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

	// 2. Build the stateless MCP server.
	srv := newStatelessServer(grpcClient, nil, nil)

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
