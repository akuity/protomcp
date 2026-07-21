// Package tasks_test — outgoing-metadata forwarding coverage. The
// generated handlers apply GRPCData.Metadata to the upstream call
// context through protomcp.OutgoingContext, which MERGES with any
// outgoing metadata already on the context instead of replacing it.
// These tests lock both halves of that contract for every generated
// surface: tool calls, resource reads, and resource lists.
package tasks_test

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	tasksserver "github.com/akuity/protomcp/examples/tasks/server"
	tasksv1 "github.com/akuity/protomcp/pkg/api/gen/examples/tasks/v1"
	"github.com/akuity/protomcp/pkg/protomcp"
)

// metadataRecorder captures the incoming gRPC metadata of every unary
// call so tests can assert what the generated MCP handlers actually
// sent upstream.
type metadataRecorder struct {
	mu       sync.Mutex
	byMethod map[string]metadata.MD
}

func (r *metadataRecorder) intercept(
	ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		r.mu.Lock()
		r.byMethod[info.FullMethod] = md.Copy()
		r.mu.Unlock()
	}
	return handler(ctx, req)
}

func (r *metadataRecorder) get(method string) metadata.MD {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byMethod[method]
}

// startRecordingGRPC boots the Tasks service with a metadata-capturing
// interceptor on a random loopback port.
func startRecordingGRPC(t *testing.T) (tasksv1.TasksClient, *metadataRecorder) {
	t.Helper()
	rec := &metadataRecorder{byMethod: map[string]metadata.MD{}}
	lis, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(rec.intercept))
	tasksv1.RegisterTasksServer(grpcSrv, tasksserver.New())
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(func() {
		grpcSrv.Stop()
		_ = lis.Close()
	})
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return tasksv1.NewTasksClient(conn), rec
}

// TestToolCall_MergesAmbientOutgoingMetadata locks the merge half of
// the contract on the tool surface: metadata already on the handler
// context (here set by an outer ToolMiddleware, standing in for
// consumer HTTP middleware or an ambient client wrapper) must arrive
// upstream ALONGSIDE the GRPCData.Metadata writes — the old
// metadata.NewOutgoingContext call silently dropped it.
func TestToolCall_MergesAmbientOutgoingMetadata(t *testing.T) {
	ctx := context.Background()
	grpcClient, rec := startRecordingGRPC(t)

	srv := protomcp.New("tasks", "0.1.0",
		protomcp.WithToolMiddleware(func(next protomcp.ToolHandler) protomcp.ToolHandler {
			return func(ctx context.Context, req *mcp.CallToolRequest, g *protomcp.GRPCData) (*mcp.CallToolResult, error) {
				ctx = metadata.AppendToOutgoingContext(ctx, "x-ambient", "outer-layer")
				g.SetMetadata("x-middleware", "tool-mw")
				return next(ctx, req, g)
			}
		}),
	)
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)
	cs := connect(ctx, t, srv)

	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Tasks_ListTasks",
		Arguments: map[string]any{},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	md := rec.get(tasksv1.Tasks_ListTasks_FullMethodName)
	if md == nil {
		t.Fatal("upstream ListTasks saw no metadata")
	}
	if got := md.Get("x-ambient"); len(got) != 1 || got[0] != "outer-layer" {
		t.Errorf("x-ambient = %v, want [outer-layer] (ambient outgoing metadata was clobbered)", got)
	}
	if got := md.Get("x-middleware"); len(got) != 1 || got[0] != "tool-mw" {
		t.Errorf("x-middleware = %v, want [tool-mw]", got)
	}
}

// TestResourceRead_ForwardsMiddlewareMetadata locks the forwarding half
// on the resource-read surface: GRPCData.Metadata written by a
// WithResourceReadMiddleware must reach the upstream gRPC server. The
// old generated code initialized Metadata to nil and never applied it,
// silently dropping every write.
func TestResourceRead_ForwardsMiddlewareMetadata(t *testing.T) {
	ctx := context.Background()
	grpcClient, rec := startRecordingGRPC(t)

	created, err := grpcClient.CreateTask(ctx, &tasksv1.CreateTaskRequest{
		Task: &tasksv1.Task{Title: "metadata probe"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	srv := protomcp.New("tasks", "0.1.0",
		protomcp.WithResourceReadMiddleware(func(next protomcp.ResourceReadHandler) protomcp.ResourceReadHandler {
			return func(ctx context.Context, req *mcp.ReadResourceRequest, g *protomcp.GRPCData) (*mcp.ReadResourceResult, error) {
				ctx = metadata.AppendToOutgoingContext(ctx, "x-ambient", "outer-layer")
				g.SetMetadata("x-middleware", "read-mw")
				return next(ctx, req, g)
			}
		}),
	)
	tasksv1.RegisterTasksMCPResources(srv, grpcClient)
	cs := connect(ctx, t, srv)

	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{
		URI: "tasks://" + created.GetId(),
	}); err != nil {
		t.Fatalf("ReadResource: %v", err)
	}

	md := rec.get(tasksv1.Tasks_GetTask_FullMethodName)
	if md == nil {
		t.Fatal("upstream GetTask saw no metadata")
	}
	if got := md.Get("x-middleware"); len(got) != 1 || got[0] != "read-mw" {
		t.Errorf("x-middleware = %v, want [read-mw] (resource read dropped GRPCData.Metadata)", got)
	}
	if got := md.Get("x-ambient"); len(got) != 1 || got[0] != "outer-layer" {
		t.Errorf("x-ambient = %v, want [outer-layer] (ambient outgoing metadata was clobbered)", got)
	}
}

// TestResourceList_ForwardsMiddlewareMetadata is the resources/list
// counterpart: metadata written by a WithResourceListMiddleware must
// reach the upstream lister RPC.
func TestResourceList_ForwardsMiddlewareMetadata(t *testing.T) {
	ctx := context.Background()
	grpcClient, rec := startRecordingGRPC(t)

	srv := protomcp.New("tasks", "0.1.0",
		protomcp.WithResourceListMiddleware(func(next protomcp.ResourceListHandler) protomcp.ResourceListHandler {
			return func(ctx context.Context, req *mcp.ListResourcesRequest, g *protomcp.GRPCData) (*mcp.ListResourcesResult, error) {
				g.SetMetadata("x-middleware", "list-mw")
				return next(ctx, req, g)
			}
		}),
	)
	tasksv1.RegisterTasksMCPResources(srv, grpcClient)
	cs := connect(ctx, t, srv)

	if _, err := cs.ListResources(ctx, &mcp.ListResourcesParams{}); err != nil {
		t.Fatalf("ListResources: %v", err)
	}

	md := rec.get(tasksv1.Tasks_ListAllResources_FullMethodName)
	if md == nil {
		t.Fatal("upstream ListAllResources saw no metadata")
	}
	if got := md.Get("x-middleware"); len(got) != 1 || got[0] != "list-mw" {
		t.Errorf("x-middleware = %v, want [list-mw] (resource list dropped GRPCData.Metadata)", got)
	}
}
