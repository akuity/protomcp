// Wire-level e2e for the stateless serving shape: protocol 2026-07-28
// negotiated over plain HTTP with no session affinity, resource
// subscriptions delivered over subscriptions/listen streams, handler
// cancellation tied to the HTTP request, and legacy clients still
// served by the same endpoint.
//
// Registration is asynchronous under subscriptions/listen (the client
// dispatches the listen call without awaiting), so delivery tests
// mutate in a poll loop until the notification arrives instead of
// mutating once and hoping the subscription was registered in time.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	tasksserver "github.com/akuity/protomcp/examples/tasks/server"
	tasksv1 "github.com/akuity/protomcp/pkg/api/gen/examples/tasks/v1"
	"github.com/akuity/protomcp/pkg/protomcp"
)

// harness bundles the running stateless server with the signal
// channels the tests observe.
type harness struct {
	url          string
	grpcClient   tasksv1.TasksClient
	srv          *protomcp.Server
	subscribed   chan string // URI per SubscribeHandler invocation
	unsubscribed chan string // URI per UnsubscribeHandler invocation
}

func startHarness(t *testing.T) *harness {
	t.Helper()
	tSrv := tasksserver.New()
	grpcClient, shutdownGRPC, err := startTasksGRPC(context.Background(), tSrv)
	if err != nil {
		t.Fatalf("start grpc: %v", err)
	}
	t.Cleanup(shutdownGRPC)

	h := &harness{
		grpcClient:   grpcClient,
		subscribed:   make(chan string, 16),
		unsubscribed: make(chan string, 16),
	}
	h.srv = newStatelessServer(grpcClient,
		func(_ context.Context, req *mcp.SubscribeRequest) error {
			h.subscribed <- req.Params.URI
			return nil
		},
		func(_ context.Context, req *mcp.UnsubscribeRequest) error {
			h.unsubscribed <- req.Params.URI
			return nil
		},
	)
	tSrv.OnChange = func(id string) {
		_ = h.srv.SDK().ResourceUpdated(context.Background(),
			&mcp.ResourceUpdatedNotificationParams{URI: "tasks://" + id})
	}

	ts := httptest.NewServer(h.srv)
	t.Cleanup(ts.Close)
	h.url = ts.URL
	return h
}

// connect dials the harness with the real SDK client over Streamable
// HTTP and asserts the session negotiated the modern protocol — the
// point of stateless serving. notif receives every ResourceUpdated URI.
func (h *harness) connect(ctx context.Context, t *testing.T, notif chan<- string) *mcp.ClientSession {
	t.Helper()
	opts := &mcp.ClientOptions{}
	if notif != nil {
		opts.ResourceUpdatedHandler = func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			select {
			case notif <- req.Params.URI:
			default:
			}
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "stateless-test-client", Version: "0.0.1"}, opts)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.url}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if v := cs.InitializeResult().ProtocolVersion; v < "2026-07-28" {
		t.Fatalf("negotiated protocol %q; stateless serving must negotiate >= 2026-07-28 via server/discover", v)
	}
	return cs
}

func (h *harness) createTask(ctx context.Context, t *testing.T, title string) *tasksv1.Task {
	t.Helper()
	task, err := h.grpcClient.CreateTask(ctx, &tasksv1.CreateTaskRequest{Task: &tasksv1.Task{Title: title}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// mutateUntilNotified updates the task in a poll loop until notif
// yields its URI, absorbing the async listen registration.
func (h *harness) mutateUntilNotified(ctx context.Context, t *testing.T, task *tasksv1.Task, notif <-chan string) {
	t.Helper()
	wantURI := "tasks://" + task.Id
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case uri := <-notif:
			if uri != wantURI {
				t.Fatalf("notified for %q, want %q", uri, wantURI)
			}
			return
		case <-tick.C:
			task.Title += "."
			if _, err := h.grpcClient.UpdateTask(ctx, &tasksv1.UpdateTaskRequest{
				Id: task.Id, Title: task.Title, Description: task.Description, Done: task.Done,
			}); err != nil {
				t.Fatalf("UpdateTask: %v", err)
			}
		case <-deadline:
			t.Fatalf("no resources/updated notification for %q within 5s", wantURI)
		}
	}
}

func waitForURI(t *testing.T, ch <-chan string, want, what string) {
	t.Helper()
	select {
	case uri := <-ch:
		if uri != want {
			t.Fatalf("%s fired for %q, want %q", what, uri, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not fire for %q within 5s", what, want)
	}
}

// TestStateless_ToolCallsOnModernProtocol pins the baseline: a v1.7.0
// client against the stateless endpoint negotiates >= 2026-07-28 (the
// connect helper asserts it) and normal tool round-trips work.
func TestStateless_ToolCallsOnModernProtocol(t *testing.T) {
	ctx := context.Background()
	h := startHarness(t)
	cs := h.connect(ctx, t, nil)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Tasks_CreateTask",
		Arguments: json.RawMessage(`{"task":{"title":"stateless","done":false}}`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out.IsError {
		t.Fatalf("CallTool: unexpected IsError result: %+v", out)
	}
}

// TestStateless_SubscribeDeliversOverListen is the headline: a
// subscription made on a stateless server (where resources/subscribe
// does not exist) flows through SubscribeHandler and delivers
// resources/updated notifications over the subscriptions/listen
// stream. A second concurrent client on the same URI proves per-
// connection fan-out.
func TestStateless_SubscribeDeliversOverListen(t *testing.T) {
	ctx := context.Background()
	h := startHarness(t)

	notifA := make(chan string, 16)
	notifB := make(chan string, 16)
	csA := h.connect(ctx, t, notifA)
	csB := h.connect(ctx, t, notifB)

	task := h.createTask(ctx, t, "watched")
	uri := "tasks://" + task.Id

	if err := csA.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	waitForURI(t, h.subscribed, uri, "SubscribeHandler (A)")
	if err := csB.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	waitForURI(t, h.subscribed, uri, "SubscribeHandler (B)")

	h.mutateUntilNotified(ctx, t, task, notifA)
	// B holds its own listen stream; the same mutation burst must have
	// reached it too (allow a beat for independent delivery).
	select {
	case gotB := <-notifB:
		if gotB != uri {
			t.Fatalf("client B notified for %q, want %q", gotB, uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("client B did not receive resources/updated within 5s")
	}
}

// TestStateless_UnsubscribeTearsDown proves the other half of the
// lifecycle: Unsubscribe ends the per-URI listen stream, the server's
// UnsubscribeHandler fires, and further mutations deliver nothing.
func TestStateless_UnsubscribeTearsDown(t *testing.T) {
	ctx := context.Background()
	h := startHarness(t)

	notif := make(chan string, 16)
	cs := h.connect(ctx, t, notif)

	task := h.createTask(ctx, t, "short-lived-watch")
	uri := "tasks://" + task.Id

	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitForURI(t, h.subscribed, uri, "SubscribeHandler")
	h.mutateUntilNotified(ctx, t, task, notif)

	if err := cs.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: uri}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	waitForURI(t, h.unsubscribed, uri, "UnsubscribeHandler")

	// Drain anything in flight, then mutate and require silence.
	time.Sleep(200 * time.Millisecond)
	for {
		select {
		case <-notif:
			continue
		default:
		}
		break
	}
	task.Title += "!"
	if _, err := h.grpcClient.UpdateTask(ctx, &tasksv1.UpdateTaskRequest{
		Id: task.Id, Title: task.Title, Description: task.Description, Done: task.Done,
	}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	select {
	case uri := <-notif:
		t.Fatalf("received %q after Unsubscribe", uri)
	case <-time.After(700 * time.Millisecond):
	}
}

// TestStateless_CancellationPropagatesToHandler proves
// PropagateRequestCancellation: aborting the HTTP request mid-call
// cancels the in-flight handler context instead of letting the handler
// run to completion for a client that is gone.
func TestStateless_CancellationPropagatesToHandler(t *testing.T) {
	ctx := context.Background()
	h := startHarness(t)

	started := make(chan struct{})
	canceled := make(chan struct{})
	h.srv.MustAddTool(&mcp.Tool{
		Name:        "wait-for-cancel",
		Description: "blocks until its context is canceled; test instrumentation",
		InputSchema: protomcp.MustParseSchema(`{"type":"object"}`),
	}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "never canceled"}}}, nil
		}
	})

	cs := h.connect(ctx, t, nil)
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = cs.CallTool(callCtx, &mcp.CallToolParams{
			Name:      "wait-for-cancel",
			Arguments: json.RawMessage(`{}`),
		})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler never started")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatalf("handler context not canceled within 3s of aborting the request")
	}
}

// TestStateless_LegacyInitializeStillServes pins backward
// compatibility: the same stateless endpoint answers a classic
// initialize from a pre-2026 client, echoing the requested legacy
// protocol version (initialize never negotiates 2026-07-28).
func TestStateless_LegacyInitializeStillServes(t *testing.T) {
	h := startHarness(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"legacy","version":"0.0.1"}}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(raw), `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize response does not echo the requested legacy protocol version: %s", raw)
	}
}

// TestStateless_RejectsGET pins the stateless transport contract:
// there is no standalone GET stream, only POSTs.
func TestStateless_RejectsGET(t *testing.T) {
	h := startHarness(t)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}
}
