// Legacy-protocol coverage for the elicitation gate. Every other
// elicitation test in this package rides in-memory transports, which
// negotiate protocol 2026-07-28 via server/discover and therefore
// exercise only the go-sdk's client-side multi-round-trip middleware.
// Pre-2026 clients instead hit the SDK's server-side compatibility shim:
// it intercepts the InputRequests result, performs a real session.Elicit
// round-trip, and re-invokes the handler with the answer. That shim is
// the entire backward-compatibility story for generated elicitation
// gates, so it gets its own suite here.
//
// Streamable HTTP without Stateless pins sessions to 2025-11-25 (the
// transport refuses to advertise 2026-07-28), which is exactly the
// legacy path. connectLegacyHTTP asserts the negotiated version so this
// suite fails loudly if the transport ever starts speaking the new
// protocol and the shim silently stops being exercised.
package tasks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tasksv1 "github.com/akuity/protomcp/pkg/api/gen/examples/tasks/v1"
	"github.com/akuity/protomcp/pkg/protomcp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// legacyProtocolCutoff is the first protocol revision on which the
// server-side elicitation shim no longer applies.
const legacyProtocolCutoff = "2026-07-28"

// connectLegacyHTTP serves srv over Streamable HTTP (non-stateless) and
// connects an MCP client to it, returning a session guaranteed to speak
// a pre-2026-07-28 protocol revision.
func connectLegacyHTTP(ctx context.Context, t *testing.T, srv *protomcp.Server, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "legacy-test-client", Version: "0.0.1"}, opts)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if v := cs.InitializeResult().ProtocolVersion; v >= legacyProtocolCutoff {
		t.Fatalf("negotiated protocol %q >= %q: this suite must exercise the legacy server-side elicitation shim", v, legacyProtocolCutoff)
	}
	return cs
}

// TestDeleteTask_LegacyShimAccept drives confirm-then-delete over the
// legacy path: the server-side shim turns the handler's InputRequests
// result into a session.Elicit round-trip, the client accepts, and the
// backend observes the Delete.
func TestDeleteTask_LegacyShimAccept(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	var seenMessage atomic.Value // string
	var elicitCalls atomic.Int32
	cs := connectLegacyHTTP(ctx, t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicitCalls.Add(1)
			if req != nil && req.Params != nil {
				seenMessage.Store(req.Params.Message)
			}
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})

	var created tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask",
		`{"task":{"title":"delete-me-legacy","done":false}}`, &created)
	if created.Id == "" {
		t.Fatalf("Create: empty id")
	}

	var del tasksv1.DeleteTaskResponse
	callTool(ctx, t, cs, "Tasks_DeleteTask",
		fmt.Sprintf(`{"id":%q}`, created.Id), &del)
	if !del.Existed {
		t.Errorf("Delete: Existed = false, want true (task was present before the delete)")
	}
	if got := elicitCalls.Load(); got != 1 {
		t.Errorf("elicitation handler called %d times, want 1", got)
	}
	msg, _ := seenMessage.Load().(string)
	if !strings.Contains(msg, created.Id) {
		t.Errorf("elicitation message %q does not contain task id %q", msg, created.Id)
	}
}

// TestDeleteTask_LegacyShimDecline asserts the decline path through the
// shim: an IsError tool result (not a protocol error) and an untouched
// backend.
func TestDeleteTask_LegacyShimDecline(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	cs := connectLegacyHTTP(ctx, t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})

	var created tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask",
		`{"task":{"title":"keep-me-legacy","done":false}}`, &created)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Tasks_DeleteTask",
		Arguments: json.RawMessage(fmt.Sprintf(`{"id":%q}`, created.Id)),
	})
	if err != nil {
		t.Fatalf("CallTool: transport error: %v", err)
	}
	if !out.IsError {
		t.Fatalf("Delete: want IsError, got success: %+v", out)
	}

	// Backend untouched.
	var got tasksv1.Task
	callTool(ctx, t, cs, "Tasks_GetTask",
		fmt.Sprintf(`{"id":%q}`, created.Id), &got)
	if got.Id != created.Id {
		t.Errorf("Get after declined delete: got %q, want %q", got.Id, created.Id)
	}
}

// TestDeleteTask_LegacyShimNoHandler pins the upgrade-note behavior: a
// client with no ElicitationHandler now gets a hard protocol error from
// the elicitation round-trip — under go-sdk v1.5.0 this surfaced as a
// graceful IsError tool result instead. The backend must stay untouched
// either way.
func TestDeleteTask_LegacyShimNoHandler(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	cs := connectLegacyHTTP(ctx, t, srv, nil)

	var created tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask",
		`{"task":{"title":"keep-me-nohandler","done":false}}`, &created)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Tasks_DeleteTask",
		Arguments: json.RawMessage(fmt.Sprintf(`{"id":%q}`, created.Id)),
	})
	if err == nil {
		t.Fatalf("CallTool: want hard error without an ElicitationHandler, got result %+v", out)
	}
	if !strings.Contains(err.Error(), "elicitation") {
		t.Errorf("CallTool error %q does not mention elicitation", err)
	}

	// Backend untouched.
	var got tasksv1.Task
	callTool(ctx, t, cs, "Tasks_GetTask",
		fmt.Sprintf(`{"id":%q}`, created.Id), &got)
	if got.Id != created.Id {
		t.Errorf("Get after failed delete: got %q, want %q", got.Id, created.Id)
	}
}
