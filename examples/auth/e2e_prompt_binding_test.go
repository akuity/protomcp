package auth_test

import (
	"context"
	"testing"

	authv1 "github.com/akuity/protomcp/pkg/api/gen/examples/auth/v1"
	"github.com/akuity/protomcp/pkg/protomcp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type recordingPromptClient struct {
	calls int
	last  *authv1.TestPromptBindingRequest
}

func (c *recordingPromptClient) Render(_ context.Context, in *authv1.TestPromptBindingRequest, _ ...grpc.CallOption) (*authv1.TestPromptBindingReply, error) {
	c.calls++
	c.last = proto.Clone(in).(*authv1.TestPromptBindingRequest)
	return &authv1.TestPromptBindingReply{Text: "rendered"}, nil
}

func connectPromptClient(ctx context.Context, t *testing.T, upstream authv1.TestPromptBindingClient) *mcp.ClientSession {
	t.Helper()
	srv := protomcp.New("auth-prompts", "0.1.0")
	authv1.RegisterTestPromptBindingMCPPrompts(srv, upstream)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.SDK().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func TestPromptBindingArgumentsAndDecoding(t *testing.T) {
	ctx := context.Background()
	upstream := &recordingPromptClient{}
	client := connectPromptClient(ctx, t, upstream)

	list, err := client.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(list.Prompts) != 1 {
		t.Fatalf("prompts length = %d, want 1", len(list.Prompts))
	}
	arguments := list.Prompts[0].Arguments
	if len(arguments) != 3 {
		t.Fatalf("arguments length = %d, want 3", len(arguments))
	}
	for i, want := range []string{"topic", "alias", "handle"} {
		if arguments[i].Name != want {
			t.Errorf("argument %d name = %q, want %q", i, arguments[i].Name, want)
		}
	}

	result, err := client.GetPrompt(ctx, &mcp.GetPromptParams{
		Name: "TestPromptBinding_Render",
		Arguments: map[string]string{
			"topic":       "release",
			"alias":       "stable",
			"serverToken": "must-not-reach-upstream",
		},
	})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages length = %d, want 1", len(result.Messages))
	}
	if upstream.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstream.calls)
	}
	if upstream.last.Topic == nil || upstream.last.GetTopic() != "release" {
		t.Errorf("topic = %v, want present value %q", upstream.last.Topic, "release")
	}
	if upstream.last.GetAlias() != "stable" {
		t.Errorf("alias = %q, want %q", upstream.last.GetAlias(), "stable")
	}
	if upstream.last.GetServerToken() != "" {
		t.Errorf("server token reached upstream: %q", upstream.last.GetServerToken())
	}

	_, err = client.GetPrompt(ctx, &mcp.GetPromptParams{
		Name: "TestPromptBinding_Render",
		Arguments: map[string]string{
			"alias":  "one",
			"handle": "two",
		},
	})
	if err == nil {
		t.Fatal("GetPrompt with multiple oneof members succeeded, want error")
	}
	if upstream.calls != 1 {
		t.Fatalf("upstream calls after invalid oneof = %d, want 1", upstream.calls)
	}
}
