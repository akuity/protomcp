// Package tasks_test also covers the elicitation-gated DeleteTask path.
// DeleteTask carries both the destructive tool hint and a confirmation
// elicitation in tasks.proto; the generated handler must:
//
//   - publish the confirmation as a multi-round-trip InputRequest before
//     the upstream gRPC call (the SDK fulfills it through the client's
//     ElicitationHandler and re-invokes the handler with the answer)
//   - render {{id}} into the prompt so the user sees *which* task
//   - run the gRPC call only when action=="accept"
//   - return an IsError CallToolResult with a clear message when the
//     user declines, without having touched the backend
//
// The server-side tasks.Delete is idempotent and stateful, so "decline
// then re-issue with accept" is a valid assertion: the second call
// must see the task because the first call short-circuited.
package tasks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	tasksv1 "github.com/akuity/protomcp/pkg/api/gen/examples/tasks/v1"
	"github.com/akuity/protomcp/pkg/protomcp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDeleteTask_ElicitationAccept exercises the full confirm-then-delete
// path: the elicitation fires (carrying the rendered prompt), the client
// returns action=accept, and the backend observes the Delete call.
func TestDeleteTask_ElicitationAccept(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	var seenMessage atomic.Value // string
	var elicitCalls atomic.Int32
	cs := connectWith(ctx, t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicitCalls.Add(1)
			if req != nil && req.Params != nil {
				seenMessage.Store(req.Params.Message)
			}
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})

	// Seed a task so Delete has something to find. Use the public CRUD
	// surface so the test path matches what a real client does.
	var created tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask",
		`{"task":{"title":"delete-me","done":false}}`, &created)
	if created.Id == "" {
		t.Fatalf("Create: empty id")
	}

	// Delete, elicitation handler must fire before the backend sees the
	// call, and the rendered prompt must contain the task id.
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
	if !strings.Contains(msg, "Delete task with id") {
		t.Errorf("elicitation message %q is missing the confirmation prefix", msg)
	}

	// Task is really gone, a second Delete returns existed=false.
	var del2 tasksv1.DeleteTaskResponse
	callTool(ctx, t, cs, "Tasks_DeleteTask",
		fmt.Sprintf(`{"id":%q}`, created.Id), &del2)
	if del2.Existed {
		t.Errorf("second Delete: Existed = true, want false (task was already removed)")
	}
}

// TestDeleteTask_ElicitationDecline asserts that when the client returns
// action=decline the gRPC Delete does NOT run: the task survives, and the
// tool result is an IsError with the canned decline message.
func TestDeleteTask_ElicitationDecline(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	var elicitCalls atomic.Int32
	cs := connectWith(ctx, t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicitCalls.Add(1)
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})

	// Seed a task.
	var created tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask",
		`{"task":{"title":"keep-me","done":false}}`, &created)

	// Call Delete, declined elicitation must surface as an IsError
	// response, not a transport-level error, and the backend must not
	// have seen the call.
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
	if got := elicitCalls.Load(); got != 1 {
		t.Errorf("elicitation handler called %d times, want 1", got)
	}
	if len(out.Content) == 0 {
		t.Fatalf("Delete: IsError result has no content")
	}
	text, _ := out.Content[0].(*mcp.TextContent)
	if text == nil {
		t.Fatalf("Delete: first content block is not TextContent: %T", out.Content[0])
	}
	if !strings.Contains(text.Text, "declined") {
		t.Errorf("Delete: decline message %q does not mention decline", text.Text)
	}

	// Task is still there, confirm via Get.
	var got tasksv1.Task
	callTool(ctx, t, cs, "Tasks_GetTask",
		fmt.Sprintf(`{"id":%q}`, created.Id), &got)
	if got.Id != created.Id {
		t.Errorf("Get after declined delete: got %q, want %q", got.Id, created.Id)
	}
	if got.Title != "keep-me" {
		t.Errorf("Get after declined delete: title=%q, want 'keep-me'", got.Title)
	}
}

// TestDeleteTask_ElicitationReusedParamsRePrompts is the regression test
// for answer/request binding. The SDK's client middleware mutates the
// caller's CallToolParams in place on a fulfilled elicitation
// (setMultiRoundTripRetryParams), so a client that reuses one params
// struct across calls carries a stale inputResponses["confirm"] into the
// next call. Without RequestState binding that stale answer silently
// confirmed a delete of a *different* task; with it, every distinct call
// re-prompts.
func TestDeleteTask_ElicitationReusedParamsRePrompts(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	var elicitCalls atomic.Int32
	cs := connectWith(ctx, t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicitCalls.Add(1)
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})

	var first, second tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask", `{"task":{"title":"one","done":false}}`, &first)
	callTool(ctx, t, cs, "Tasks_CreateTask", `{"task":{"title":"two","done":false}}`, &second)

	// One params struct, reused across both deletes: after the first
	// call the SDK has stuffed InputResponses + RequestState into it.
	params := &mcp.CallToolParams{
		Name:      "Tasks_DeleteTask",
		Arguments: json.RawMessage(fmt.Sprintf(`{"id":%q}`, first.Id)),
	}
	if out, err := cs.CallTool(ctx, params); err != nil || out.IsError {
		t.Fatalf("first Delete: err=%v out=%+v", err, out)
	}
	if got := elicitCalls.Load(); got != 1 {
		t.Fatalf("after first delete: elicitation handler called %d times, want 1", got)
	}

	params.Arguments = json.RawMessage(fmt.Sprintf(`{"id":%q}`, second.Id))
	if out, err := cs.CallTool(ctx, params); err != nil || out.IsError {
		t.Fatalf("second Delete: err=%v out=%+v", err, out)
	}
	if got := elicitCalls.Load(); got != 2 {
		t.Errorf("after second delete: elicitation handler called %d times, want 2 (stale answer must re-prompt, not confirm)", got)
	}
}

// TestDeleteTask_ElicitationIdenticalReplayRePrompts pins the
// byte-identical replay property end to end: re-issuing the SAME
// CallToolParams struct — same tool, same arguments — after a confirmed
// call must prompt again rather than ride the previous answer. The
// server-side RequestState is a recomputable content hash, so this
// protection lives client-side: a fixed SDK never writes retry state
// into the caller's params, the replay arrives with no answer, and the
// gate re-prompts. go-sdk <= v1.7.0 instead leaves the fulfilled answer
// and matching state on the caller's struct
// (modelcontextprotocol/go-sdk#1144), so the replay skips the prompt.
// On detecting the mutation this test skips — but the skip is locked
// to the audited go-sdk v1.7.0 pin: on any other version the same
// detection is a hard failure, so a pin bump either enforces the
// re-prompt (SDK fixed by modelcontextprotocol/go-sdk#1145) or fails
// loudly (still broken) instead of skipping.
func TestDeleteTask_ElicitationIdenticalReplayRePrompts(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	// Accept the first prompt, decline any later one: if the replay
	// correctly re-prompts, it must then short-circuit with IsError.
	var elicitCalls atomic.Int32
	cs := connectWith(ctx, t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if elicitCalls.Add(1) == 1 {
				return &mcp.ElicitResult{Action: "accept"}, nil
			}
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})

	var created tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask", `{"task":{"title":"replay-me","done":false}}`, &created)

	params := &mcp.CallToolParams{
		Name:      "Tasks_DeleteTask",
		Arguments: json.RawMessage(fmt.Sprintf(`{"id":%q}`, created.Id)),
	}
	if out, err := cs.CallTool(ctx, params); err != nil || out.IsError {
		t.Fatalf("first Delete: err=%v out=%+v", err, out)
	}
	if got := elicitCalls.Load(); got != 1 {
		t.Fatalf("after first delete: elicitation handler called %d times, want 1", got)
	}

	if params.RequestState != "" || params.InputResponses != nil {
		if v := goSDKVersion(t); v == "v1.7.0" {
			t.Skipf("go-sdk %s leaves multi-round-trip state on caller params (modelcontextprotocol/go-sdk#1144, fixed by #1145): a byte-identical replay would skip the prompt; skip locked to this exact version", v)
		} else {
			t.Fatalf("go-sdk %s still leaves multi-round-trip state on caller params (modelcontextprotocol/go-sdk#1144): the replay window is open on a version other than the audited v1.7.0 — hold the bump for a release containing modelcontextprotocol/go-sdk#1145, or re-lock deliberately", v)
		}
	}

	// Replay the SAME struct, byte-identical arguments included.
	out, err := cs.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("replayed Delete: transport error: %v", err)
	}
	if got := elicitCalls.Load(); got != 2 {
		t.Errorf("after replay: elicitation handler called %d times, want 2 (identical replay must re-prompt)", got)
	}
	if !out.IsError {
		t.Errorf("replayed Delete: want IsError (user declined the re-prompt), got success: %+v", out)
	}
}

// goSDKVersion returns the go-sdk module version the build resolves
// (the replace target's version when a replace is in effect). Test
// binaries embed no module dep info, so ask the go tool.
func goSDKVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f",
		"{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}",
		"github.com/modelcontextprotocol/go-sdk").Output()
	if err != nil {
		t.Fatalf("resolving go-sdk version: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestDeleteTask_ElicitationPrePopulatedAnswerRePrompts asserts that an
// inputResponses entry supplied on the very first call, without the
// matching RequestState, does not skip the confirmation: the gate
// re-prompts, and a declining user still blocks the delete.
func TestDeleteTask_ElicitationPrePopulatedAnswerRePrompts(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	var elicitCalls atomic.Int32
	cs := connectWith(ctx, t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicitCalls.Add(1)
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})

	var created tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask", `{"task":{"title":"keep-me","done":false}}`, &created)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Tasks_DeleteTask",
		Arguments: json.RawMessage(fmt.Sprintf(`{"id":%q}`, created.Id)),
		InputResponses: mcp.InputResponseMap{
			"confirm": &mcp.ElicitResult{Action: "accept"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: transport error: %v", err)
	}
	if !out.IsError {
		t.Fatalf("Delete: want IsError (user declined the re-prompt), got success: %+v", out)
	}
	if got := elicitCalls.Load(); got != 1 {
		t.Errorf("elicitation handler called %d times, want 1 (pre-populated answer must trigger a real prompt)", got)
	}

	// Task survived.
	var got tasksv1.Task
	callTool(ctx, t, cs, "Tasks_GetTask",
		fmt.Sprintf(`{"id":%q}`, created.Id), &got)
	if got.Id != created.Id {
		t.Errorf("Get after blocked delete: got %q, want %q", got.Id, created.Id)
	}
}

// TestDeleteTask_ElicitationCancel asserts that action=cancel behaves the
// same as decline, it is a non-accept action, so the tool must
// short-circuit with IsError and leave the backend untouched.
func TestDeleteTask_ElicitationCancel(t *testing.T) {
	ctx := context.Background()
	grpcClient := startGRPC(t)
	srv := protomcp.New("tasks", "0.1.0")
	tasksv1.RegisterTasksMCPTools(srv, grpcClient)

	cs := connectWith(ctx, t, srv, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "cancel"}, nil
		},
	})

	var created tasksv1.Task
	callTool(ctx, t, cs, "Tasks_CreateTask",
		`{"task":{"title":"keep-me","done":false}}`, &created)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Tasks_DeleteTask",
		Arguments: json.RawMessage(fmt.Sprintf(`{"id":%q}`, created.Id)),
	})
	if err != nil {
		t.Fatalf("CallTool: transport error: %v", err)
	}
	if !out.IsError {
		t.Fatalf("Delete: want IsError on cancel, got success: %+v", out)
	}

	// Task survived.
	var got tasksv1.Task
	callTool(ctx, t, cs, "Tasks_GetTask",
		fmt.Sprintf(`{"id":%q}`, created.Id), &got)
	if got.Id != created.Id {
		t.Errorf("Get after canceled delete: got %q, want %q", got.Id, created.Id)
	}
}
