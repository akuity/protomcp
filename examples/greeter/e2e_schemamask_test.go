package greeter_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"

	greeterv1 "github.com/akuity/protomcp/pkg/api/gen/examples/greeter/v1"
	"github.com/akuity/protomcp/pkg/protomcp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type capturingGreeter struct {
	greeterv1.UnimplementedGreeterServer

	mu       sync.Mutex
	lastEcho *greeterv1.EchoComplexRequest
}

func (s *capturingGreeter) EchoComplex(_ context.Context, req *greeterv1.EchoComplexRequest) (*greeterv1.EchoComplexResponse, error) {
	s.mu.Lock()
	s.lastEcho = req
	s.mu.Unlock()
	return &greeterv1.EchoComplexResponse{
		Name:         req.GetName(),
		InternalNote: "server-secret",
	}, nil
}

func (s *capturingGreeter) received() *greeterv1.EchoComplexRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastEcho
}

func startCapturingGRPC(t *testing.T) (greeterv1.GreeterClient, *capturingGreeter) {
	t.Helper()
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	impl := &capturingGreeter{}
	grpcSrv := grpc.NewServer()
	greeterv1.RegisterGreeterServer(grpcSrv, impl)
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
	return greeterv1.NewGreeterClient(conn), impl
}

func TestSchemaMaskHidesExcludedFieldFromSchemas(t *testing.T) {
	grpcClient, _ := startCapturingGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, tool := range list.Tools {
		if tool.Name != "Greeter_EchoComplex" {
			continue
		}
		raw, mErr := json.Marshal(tool)
		if mErr != nil {
			t.Fatalf("marshal tool: %v", mErr)
		}
		if strings.Contains(string(raw), "internalNote") {
			t.Errorf("excluded field internalNote leaked into advertised schemas:\n%s", raw)
		}
		return
	}
	t.Fatal("Greeter_EchoComplex not found in tools/list")
}

func TestSchemaMaskStripsExcludedFieldBothDirections(t *testing.T) {
	grpcClient, impl := startCapturingGRPC(t)
	srv := protomcp.New("greeter", "0.1.0")
	greeterv1.RegisterGreeterMCPTools(srv, grpcClient)

	ctx := context.Background()
	cs := connect(ctx, t, srv, nil)

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "Greeter_EchoComplex",
		Arguments: json.RawMessage(`{"name":"world","internalNote":"client-leak"}`),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected IsError: %+v", out)
	}

	received := impl.received()
	if received == nil {
		t.Fatal("gRPC server never received the request")
	}
	if got := received.GetInternalNote(); got != "" {
		t.Errorf("client-supplied excluded field reached the gRPC server: %q", got)
	}
	if received.GetName() != "world" {
		t.Errorf("name = %q, want %q", received.GetName(), "world")
	}

	payload, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(payload), "server-secret") {
		t.Errorf("server-set excluded field leaked to the MCP client:\n%s", payload)
	}
	if strings.Contains(string(payload), "internalNote") {
		t.Errorf("excluded field NAME leaked to the MCP client (EmitDefaultValues re-emits cleared fields):\n%s", payload)
	}
	if !strings.Contains(string(payload), "world") {
		t.Errorf("expected echoed name in result:\n%s", payload)
	}
}
