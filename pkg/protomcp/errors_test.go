package protomcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDefaultErrorHandlerAuthEscalates verifies that Unauthenticated /
// PermissionDenied / Canceled / DeadlineExceeded gRPC statuses are
// surfaced as JSON-RPC errors wrapped in *jsonrpc.Error, the SDK only
// treats an error as a protocol-level failure when it is a *jsonrpc.Error;
// any other error type gets folded into an IsError CallToolResult.
func TestDefaultErrorHandlerAuthEscalates(t *testing.T) {
	cases := []codes.Code{
		codes.Unauthenticated,
		codes.PermissionDenied,
		codes.Canceled,
		codes.DeadlineExceeded,
	}
	for _, c := range cases {
		t.Run(c.String(), func(t *testing.T) {
			err := status.Error(c, "nope")
			res, gotErr := DefaultToolErrorHandler(context.Background(), &mcp.CallToolRequest{}, err)
			if res != nil {
				t.Fatalf("expected nil result for %s, got %+v", c, res)
			}
			if gotErr == nil {
				t.Fatalf("expected non-nil error for %s", c)
			}
			var je *jsonrpc.Error
			if !errors.As(gotErr, &je) {
				t.Fatalf("err = %T, want *jsonrpc.Error", gotErr)
			}
			if !strings.Contains(je.Message, c.String()) {
				t.Errorf("jsonrpc.Error.Message = %q, want to contain %q", je.Message, c.String())
			}
			if !strings.Contains(je.Message, "nope") {
				t.Errorf("jsonrpc.Error.Message = %q, want to contain %q", je.Message, "nope")
			}
		})
	}
}

// TestDefaultErrorHandlerOtherGRPCStatus verifies that non-auth gRPC
// statuses (e.g. NotFound) are folded into a CallToolResult with
// IsError=true, a human-readable text content, and a structured content
// carrying the serialized google.rpc.Status.
func TestDefaultErrorHandlerOtherGRPCStatus(t *testing.T) {
	err := status.Error(codes.NotFound, "missing widget")
	res, gotErr := DefaultToolErrorHandler(context.Background(), &mcp.CallToolRequest{}, err)
	if gotErr != nil {
		t.Fatalf("expected nil error, got %v", gotErr)
	}
	if res == nil {
		t.Fatalf("expected non-nil result")
	}
	if !res.IsError {
		t.Fatalf("IsError = false, want true")
	}
	if len(res.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.Contains(tc.Text, "NotFound") || !strings.Contains(tc.Text, "missing widget") {
		t.Fatalf("text = %q, want to contain code and message", tc.Text)
	}
	raw, ok := res.StructuredContent.(json.RawMessage)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want json.RawMessage", res.StructuredContent)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("StructuredContent not valid JSON: %v", err)
	}
}

func TestDefaultErrorHandlerEscalatedCodesStayDistinguishable(t *testing.T) {
	wantCodes := map[codes.Code]int64{
		codes.Unauthenticated:  -32001,
		codes.PermissionDenied: -32002,
		codes.Canceled:         -32003,
		codes.DeadlineExceeded: -32004,
	}
	for c, want := range wantCodes {
		t.Run(c.String(), func(t *testing.T) {
			_, gotErr := DefaultToolErrorHandler(context.Background(), &mcp.CallToolRequest{}, status.Error(c, "nope"))
			var je *jsonrpc.Error
			if !errors.As(gotErr, &je) {
				t.Fatalf("err = %T, want *jsonrpc.Error", gotErr)
			}
			if je.Code != want {
				t.Errorf("jsonrpc code for %s = %d, want %d", c, je.Code, want)
			}
		})
	}
}

func TestDefaultErrorHandlerNotFoundInvalidArgumentDistinguishable(t *testing.T) {
	wantCodes := map[codes.Code]float64{
		codes.NotFound:        float64(codes.NotFound),
		codes.InvalidArgument: float64(codes.InvalidArgument),
	}
	for c, want := range wantCodes {
		t.Run(c.String(), func(t *testing.T) {
			res, gotErr := DefaultToolErrorHandler(context.Background(), &mcp.CallToolRequest{}, status.Error(c, "boom"))
			if gotErr != nil {
				t.Fatalf("expected nil error, got %v", gotErr)
			}
			if res == nil || !res.IsError {
				t.Fatalf("expected IsError result, got %+v", res)
			}
			tc, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("content[0] = %T, want *mcp.TextContent", res.Content[0])
			}
			if !strings.HasPrefix(tc.Text, c.String()+":") {
				t.Errorf("text = %q, want prefix %q", tc.Text, c.String()+":")
			}
			raw, ok := res.StructuredContent.(json.RawMessage)
			if !ok {
				t.Fatalf("StructuredContent type = %T, want json.RawMessage", res.StructuredContent)
			}
			var st struct {
				Code    float64 `json:"code"`
				Message string  `json:"message"`
			}
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatalf("StructuredContent not valid JSON: %v", err)
			}
			if st.Code != want {
				t.Errorf("google.rpc.Status code = %v, want %v", st.Code, want)
			}
			if st.Message != "boom" {
				t.Errorf("google.rpc.Status message = %q, want %q", st.Message, "boom")
			}
		})
	}
}

func TestGRPCErrorToJSONRPCDistinguishability(t *testing.T) {
	toJE := func(c codes.Code) *jsonrpc.Error {
		t.Helper()
		err := grpcErrorToJSONRPC(status.Error(c, "boom"))
		var je *jsonrpc.Error
		if !errors.As(err, &je) {
			t.Fatalf("grpcErrorToJSONRPC(%s) = %T, want *jsonrpc.Error", c, err)
		}
		return je
	}
	perm := toJE(codes.PermissionDenied)
	notFound := toJE(codes.NotFound)
	invalid := toJE(codes.InvalidArgument)

	if perm.Code == notFound.Code || perm.Code == invalid.Code {
		t.Errorf("PermissionDenied code %d collides with NotFound %d / InvalidArgument %d",
			perm.Code, notFound.Code, invalid.Code)
	}
	if !strings.HasPrefix(notFound.Message, codes.NotFound.String()+":") {
		t.Errorf("NotFound message = %q, want %q prefix", notFound.Message, codes.NotFound.String()+":")
	}
	if !strings.HasPrefix(invalid.Message, codes.InvalidArgument.String()+":") {
		t.Errorf("InvalidArgument message = %q, want %q prefix", invalid.Message, codes.InvalidArgument.String()+":")
	}
}

// TestDefaultErrorHandlerPlainError verifies plain Go errors become a
// CallToolResult with IsError=true and the error message as text.
func TestDefaultErrorHandlerPlainError(t *testing.T) {
	err := errors.New("boom")
	res, gotErr := DefaultToolErrorHandler(context.Background(), &mcp.CallToolRequest{}, err)
	if gotErr != nil {
		t.Fatalf("expected nil error, got %v", gotErr)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError result, got %+v", res)
	}
	if len(res.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	if tc.Text != "boom" {
		t.Fatalf("text = %q, want %q", tc.Text, "boom")
	}
	if res.StructuredContent != nil {
		t.Fatalf("StructuredContent = %v, want nil for plain error", res.StructuredContent)
	}
}

// TestServerHandleErrorAdaptsShapes verifies Server.HandleError routes
// each error shape to the expected return values.
func TestServerHandleErrorAdaptsShapes(t *testing.T) {
	s := New("t", "0.0.1")

	// Plain error path: expect a CallToolResult, nil err.
	res, err := s.HandleToolError(context.Background(), &mcp.CallToolRequest{}, errors.New("bad"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError result, got %+v", res)
	}

	// Auth error path: expect no result but a JSON-RPC error.
	authErr := status.Error(codes.Unauthenticated, "deny")
	res, err = s.HandleToolError(context.Background(), &mcp.CallToolRequest{}, authErr)
	if res != nil {
		t.Fatalf("res = %+v, want nil", res)
	}
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}

	// Nil error path: both returns nil.
	res, err = s.HandleToolError(context.Background(), &mcp.CallToolRequest{}, nil)
	if res != nil || err != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", res, err)
	}
}

// TestFinishToolCall_FallbackOnNilNilFromHandler verifies that when a
// custom ToolErrorHandler returns (nil, nil), FinishToolCall does NOT
// panic, it falls back to the original error so the caller always
// sees a real response. Parallel assertions exist in prompt_test.go
// and resource_test.go for the other three finishers.
func TestFinishToolCall_FallbackOnNilNilFromHandler(t *testing.T) {
	buggy := func(_ context.Context, _ *mcp.CallToolRequest, _ error) (*mcp.CallToolResult, error) {
		return nil, nil
	}
	sentinel := errors.New("boom")
	s := New("t", "0.0.1", WithToolErrorHandler(buggy))
	res, _, gotErr := s.FinishToolCall(context.Background(), &mcp.CallToolRequest{}, nil, nil, sentinel)
	if res != nil {
		t.Errorf("res = %+v, want nil (fell-back path returns err, no result)", res)
	}
	if !errors.Is(gotErr, sentinel) {
		t.Errorf("err = %v, want the original sentinel", gotErr)
	}
}

// TestWithErrorHandlerOverrides verifies a custom ToolErrorHandler is
// preferred over the default.
func TestWithErrorHandlerOverrides(t *testing.T) {
	called := false
	custom := func(ctx context.Context, req *mcp.CallToolRequest, err error) (*mcp.CallToolResult, error) {
		called = true
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "custom"}}}, nil
	}
	s := New("t", "0.0.1", WithToolErrorHandler(custom))
	res, err := s.HandleToolError(context.Background(), &mcp.CallToolRequest{}, errors.New("x"))
	if !called {
		t.Fatalf("custom handler not called")
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	tc := res.Content[0].(*mcp.TextContent)
	if tc.Text != "custom" {
		t.Fatalf("text = %q, want custom", tc.Text)
	}
}
