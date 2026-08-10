// Package greeter_test exercises the greeter example end-to-end. It covers
// the two generation paths that protoc-gen-mcp supports:
//   - a unary RPC (SayHello) exposed as a tool that returns a single result
//   - a server-streaming RPC (StreamGreetings) exposed as a tool that emits
//     one MCP progress notification per gRPC message and returns a summary
//
// It also asserts that the unannotated RPC (Internal) is NOT registered as
// a tool, proving the default-deny annotation policy.
package greeter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	greeterserver "github.com/akuity/protomcp/examples/greeter/server"
	greeterv1 "github.com/akuity/protomcp/pkg/api/gen/examples/greeter/v1"
	"github.com/akuity/protomcp/pkg/protomcp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// startGRPC boots the Greeter service on a random port.
func startGRPC(t *testing.T) greeterv1.GreeterClient {
	t.Helper()
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer()
	greeterv1.RegisterGreeterServer(grpcSrv, greeterserver.New())
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
	return greeterv1.NewGreeterClient(conn)
}

// connect wires an in-memory MCP client to srv and returns the client session.
func connect(ctx context.Context, t *testing.T, srv *protomcp.Server, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, opts)
	cT, sT := mcp.NewInMemoryTransports()
	ss, err := srv.SDK().Connect(ctx, sT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := client.Connect(ctx, cT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestUnaryTool drives the SayHello tool through the SDK and verifies
// the response round-trips correctly.
func TestUnaryTool(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_SayHello",
		Arguments: json.RawMessage(`{"name":"world"}`),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected IsError: %+v", out)
	}
	text := out.Content[0].(*mcp.TextContent).Text
	var resp struct{ Message string }
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, text)
	}
	if resp.Message != "Hello, world!" {
		t.Errorf("got %q, want %q", resp.Message, "Hello, world!")
	}
}

// TestToolsListSkipsUnannotated verifies that the unannotated Internal RPC
// does NOT appear in tools/list, that is the acceptance test for default-deny.
func TestToolsListSkipsUnannotated(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := map[string]bool{}
	for _, tt := range list.Tools {
		names[tt.Name] = true
	}
	if !names["Greeter_SayHello"] {
		t.Errorf("SayHello missing from tools/list (got %v)", names)
	}
	if !names["Greeter_StreamGreetings"] {
		t.Errorf("StreamGreetings missing from tools/list (got %v)", names)
	}
	if names["Greeter_Internal"] {
		t.Errorf("unannotated Internal RPC leaked into tools/list")
	}
}

// TestServerStreamingEmitsProgress drives the streaming tool and verifies
// each gRPC message arrives as an MCP notifications/progress event, with
// the final CallToolResult summarizing the run.
func TestServerStreamingEmitsProgress(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	var (
		mu       sync.Mutex
		progress []string
		counters []float64
	)
	opts := &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, p *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()
			progress = append(progress, p.Params.Message)
			counters = append(counters, p.Params.Progress)
		},
	}

	ctx := context.Background()
	cs := connect(ctx, t, srv, opts)

	turns := int32(3)
	token := "greeter-progress-1"
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_StreamGreetings",
		Arguments: json.RawMessage(fmt.Sprintf(`{"name":"world","turns":%d}`, turns)),
		Meta:      mcp.Meta{"progressToken": token},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected IsError: %+v", out)
	}

	// Progress notifications are delivered asynchronously through the SDK's
	// client session; CallTool may return before the in-memory transport has
	// drained the last couple of notifications. Poll briefly until we see
	// the expected count (or give up after a reasonable window).
	var got []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got = append([]string(nil), progress...)
		mu.Unlock()
		if len(got) >= int(turns) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != int(turns) {
		t.Errorf("progress events: got %d, want %d (msgs=%v)", len(got), turns, got)
	}
	// Every progress message should parse as a HelloReply JSON object.
	for i, m := range got {
		var r struct{ Message string }
		if err := json.Unmarshal([]byte(m), &r); err != nil {
			t.Errorf("progress[%d] not JSON: %v (%q)", i, err, m)
		}
	}

	// Spec: Progress MUST increase with each notification. Assert strict
	// monotonic increase (the generator sets it to 1, 2, 3, ...).
	mu.Lock()
	gotCounters := append([]float64(nil), counters...)
	mu.Unlock()
	for i := 1; i < len(gotCounters); i++ {
		if gotCounters[i] <= gotCounters[i-1] {
			t.Errorf("progress counter not monotonic: [%d]=%v [%d]=%v (full=%v)",
				i-1, gotCounters[i-1], i, gotCounters[i], gotCounters)
			break
		}
	}
	_ = out // silence the out-unused warning in the non-race branch
}

func TestHTTPBodyStreamToolReassemblesTheDocument(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	turns := 5
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_DownloadTranscript",
		Arguments: json.RawMessage(fmt.Sprintf(`{"name":"world","turns":%d}`, turns)),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected IsError: %+v", out)
	}

	want := ""
	for i := 1; i <= turns; i++ {
		want += fmt.Sprintf("Turn %d: hello, world!\n", i)
	}
	if len(out.Content) != 1 {
		t.Fatalf("content items: got %d, want 1 (%+v)", len(out.Content), out.Content)
	}
	text, ok := out.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type: got %T, want *mcp.TextContent", out.Content[0])
	}
	if text.Text != want {
		t.Errorf("document: got %q, want %q", text.Text, want)
	}
	if out.StructuredContent != nil {
		t.Errorf("document tool must not return structured content, got %v", out.StructuredContent)
	}
}

func TestHTTPBodyStreamToolAdvertisesNoOutputSchema(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, tt := range list.Tools {
		if tt.Name != "Greeter_DownloadTranscript" {
			continue
		}
		if tt.OutputSchema != nil {
			t.Errorf("document tool must advertise no output schema, got %v", tt.OutputSchema)
		}
		if tt.InputSchema == nil {
			t.Errorf("document tool must still advertise its input schema")
		}
		return
	}
	t.Fatalf("Greeter_DownloadTranscript missing from tools/list")
}

func TestHTTPBodyStreamToolRejectsNonUTF8(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_DownloadTranscript",
		Arguments: json.RawMessage(`{"name":"world","binary":true}`),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !out.IsError {
		t.Fatalf("a non-UTF-8 payload must produce a tool error, got %+v", out)
	}
	text, ok := out.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type: got %T, want *mcp.TextContent", out.Content[0])
	}
	if !strings.Contains(text.Text, "valid UTF-8") {
		t.Errorf("error text should name the UTF-8 policy, got %q", text.Text)
	}
}

func TestHTTPBodyStreamToolMapsMediaContentTypes(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_DownloadTranscript",
		Arguments: json.RawMessage(`{"name":"world","binary":true,"contentType":"image/png"}`),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.IsError {
		t.Fatalf("a media payload must not be rejected for being non-UTF-8, got %+v", out)
	}
	img, ok := out.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("content type: got %T, want *mcp.ImageContent", out.Content[0])
	}
	if img.MIMEType != "image/png" {
		t.Errorf("MIMEType: got %q, want %q", img.MIMEType, "image/png")
	}
	if !bytes.Equal(img.Data, []byte{0xff, 0xfe, 0x00, 0x01}) {
		t.Errorf("image data: got %v, want the server's raw bytes", img.Data)
	}
}

func TestHTTPBodyStreamToolReportsCumulativeByteProgress(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	var (
		mu       sync.Mutex
		progress []string
		counters []float64
	)
	opts := &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, p *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()
			progress = append(progress, p.Params.Message)
			counters = append(counters, p.Params.Progress)
		},
	}

	ctx := context.Background()
	cs := connect(ctx, t, srv, opts)

	turns := 3
	transcript := ""
	for i := 1; i <= turns; i++ {
		transcript += fmt.Sprintf("Turn %d: hello, %s!\n", i, "world")
	}
	const chunkSize = 10
	wantChunks := (len(transcript) + chunkSize - 1) / chunkSize

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_DownloadTranscript",
		Arguments: json.RawMessage(fmt.Sprintf(`{"name":"world","turns":%d}`, turns)),
		Meta:      mcp.Meta{"progressToken": "transcript-progress-1"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected IsError: %+v", out)
	}

	var got []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got = append([]string(nil), progress...)
		mu.Unlock()
		if len(got) >= wantChunks {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != wantChunks {
		t.Fatalf("progress events: got %d, want %d (msgs=%v)", len(got), wantChunks, got)
	}
	if last := got[len(got)-1]; last != fmt.Sprintf("%d bytes", len(transcript)) {
		t.Errorf("final progress message: got %q, want %q", last, fmt.Sprintf("%d bytes", len(transcript)))
	}
	for i, m := range got {
		if !strings.HasSuffix(m, " bytes") {
			t.Errorf("progress[%d] should report cumulative bytes, got %q", i, m)
		}
	}

	mu.Lock()
	gotCounters := append([]float64(nil), counters...)
	mu.Unlock()
	for i := 1; i < len(gotCounters); i++ {
		if gotCounters[i] <= gotCounters[i-1] {
			t.Errorf("progress counter not monotonic: [%d]=%v [%d]=%v (full=%v)",
				i-1, gotCounters[i-1], i, gotCounters[i], gotCounters)
			break
		}
	}
}

func TestHTTPBodyStreamToolEnforcesTheDocumentLimit(t *testing.T) {
	grpcClient := startGRPC(t)
	srv := protomcp.New("greeter", "0.1.0", protomcp.WithHTTPBodyDocumentLimit(32))
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_DownloadTranscript",
		Arguments: json.RawMessage(`{"name":"world","turns":5}`),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !out.IsError {
		t.Fatalf("a document above the limit must produce a tool error, got %+v", out)
	}
	text, ok := out.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type: got %T, want *mcp.TextContent", out.Content[0])
	}
	if !strings.Contains(text.Text, "32-byte limit") {
		t.Errorf("error text should name the limit, got %q", text.Text)
	}

	within, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_DownloadTranscript",
		Arguments: json.RawMessage(`{"name":"jo","turns":1}`),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if within.IsError {
		t.Fatalf("a document within the limit must succeed, got %+v", within)
	}
}
