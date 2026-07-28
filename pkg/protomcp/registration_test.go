package protomcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testTool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}
}

// testToolHandler returns a handler whose result identifies it, so tests
// can tell which registration is bound to a name.
func testToolHandler(marker string) mcp.ToolHandlerFor[json.RawMessage, any] {
	return func(context.Context, *mcp.CallToolRequest, json.RawMessage) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: marker}}}, nil, nil
	}
}

func testResource(uri string) *mcp.Resource {
	return &mcp.Resource{URI: uri, Name: "res", MIMEType: "text/plain"}
}

func testResourceHandler(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return &mcp.ReadResourceResult{}, nil
}

// recoverPanic runs fn and returns the panic value, or nil.
func recoverPanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// TestAddToolClaimsName verifies distinct names register cleanly and are
// reported in sorted order.
func TestAddToolClaimsName(t *testing.T) {
	s := New("test", "v0")
	if err := AddTool(s, testTool("b_tool"), testToolHandler("b")); err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	if err := AddTool(s, testTool("a_tool"), testToolHandler("a")); err != nil {
		t.Fatalf("AddTool: %v", err)
	}

	got := s.RegisteredToolNames()
	if want := []string{"a_tool", "b_tool"}; !slices.Equal(got, want) {
		t.Fatalf("RegisteredToolNames() = %v, want %v", got, want)
	}
}

// TestAddToolPanicsOnDuplicateName is the core guarantee: the SDK would
// replace the first registration's handler silently, so protomcp refuses
// the second. The definitions here are byte-identical and only the
// handler differs — the shape generated registrars produce when the same
// registrar is invoked twice with different clients, and the one no
// wire-visible catalog diff can detect.
func TestAddToolRejectsDuplicateName(t *testing.T) {
	s := New("test", "v0")
	mustAdd(t, s, "dup_tool", "first")

	// The definitions here are byte-identical and only the handler differs
	// — the shape generated registrars produce when the same registrar is
	// invoked twice with different clients, and the one no wire-visible
	// catalog diff can detect.
	err := AddTool(s, testTool("dup_tool"), testToolHandler("second"))
	if err == nil {
		t.Fatalf("expected an error on duplicate tool name")
	}
	var dup *DuplicateRegistrationError
	if !errors.As(err, &dup) {
		t.Fatalf("err = %#v, want *DuplicateRegistrationError", err)
	}
	if dup.Kind != "tool" || dup.Key != "dup_tool" {
		t.Fatalf("err = %+v, want kind=tool key=dup_tool", dup)
	}
	if !strings.Contains(err.Error(), "dup_tool") {
		t.Fatalf("error %q does not name the offending tool", err)
	}

	// The first registration is still the only one recorded.
	if got := s.RegisteredToolNames(); !slices.Equal(got, []string{"dup_tool"}) {
		t.Fatalf("RegisteredToolNames() = %v, want [dup_tool]", got)
	}
}

// TestMustAddToolPanicsWithTheError verifies the Must form panics with the
// error value itself, so a caller that recovers can errors.As it rather
// than matching on message text.
func TestMustAddToolPanicsWithTheError(t *testing.T) {
	s := New("test", "v0")
	MustAddTool(s, testTool("must_dup"), testToolHandler("first"))

	recovered := recoverPanic(func() {
		MustAddTool(s, testTool("must_dup"), testToolHandler("second"))
	})
	if recovered == nil {
		t.Fatalf("expected a panic on duplicate tool name")
	}
	err, ok := recovered.(error)
	if !ok {
		t.Fatalf("panic value = %#v, want error", recovered)
	}
	var dup *DuplicateRegistrationError
	if !errors.As(err, &dup) {
		t.Fatalf("panic value %#v is not a *DuplicateRegistrationError", err)
	}
	if dup.Key != "must_dup" {
		t.Fatalf("dup.Key = %q, want must_dup", dup.Key)
	}
}

// TestAddToolPanicsOnEmptyName guards the registry key itself: an unnamed
// tool is unaddressable and would collide with the next unnamed one.
func TestAddToolRejectsEmptyName(t *testing.T) {
	s := New("test", "v0")
	if err := AddTool(s, testTool(""), testToolHandler("x")); err == nil {
		t.Fatalf("expected an error on an empty tool name")
	}
}

func TestAddToolRejectsNilTool(t *testing.T) {
	s := New("test", "v0")
	if err := AddTool(s, nil, testToolHandler("x")); err == nil {
		t.Fatalf("expected an error on a nil tool")
	}
}

// TestSnapshotDiffYieldsBatchRegistrations verifies the contract callers
// rely on to bind primitives to a scope: keys are only ever added, so the
// difference of two snapshots is exactly what the batch registered.
func TestSnapshotDiffYieldsBatchRegistrations(t *testing.T) {
	s := New("test", "v0")
	mustAdd(t, s, "existing_tool", "existing")

	before := s.RegisteredToolNames()
	mustAdd(t, s, "batch_tool_1", "1")
	mustAdd(t, s, "batch_tool_2", "2")
	after := s.RegisteredToolNames()

	var added []string
	for _, name := range after {
		if !slices.Contains(before, name) {
			added = append(added, name)
		}
	}
	if want := []string{"batch_tool_1", "batch_tool_2"}; !slices.Equal(added, want) {
		t.Fatalf("batch registrations = %v, want %v", added, want)
	}
}

// TestSDKBypassIsNotRegistered pins the escape hatch's behavior: a tool
// added straight to the SDK gets no collision protection and stays out of
// the snapshots, matching SDK()'s documented "bypasses everything".
func TestSDKBypassIsNotRegistered(t *testing.T) {
	s := New("test", "v0")
	mcp.AddTool(s.SDK(), testTool("bypass_tool"), testToolHandler("bypass"))

	if got := s.RegisteredToolNames(); len(got) != 0 {
		t.Fatalf("RegisteredToolNames() = %v, want empty", got)
	}
	// And the registry does not consider the name taken.
	if err := AddTool(s, testTool("bypass_tool"), testToolHandler("claimed")); err != nil {
		t.Fatalf("unexpected error claiming a name the SDK already holds: %v", err)
	}
}

func TestAddResourceRejectsDuplicateURI(t *testing.T) {
	s := New("test", "v0")
	if err := s.AddResource(testResource("test://x/1"), testResourceHandler); err != nil {
		t.Fatalf("AddResource: %v", err)
	}

	err := s.AddResource(testResource("test://x/1"), testResourceHandler)
	var dup *DuplicateRegistrationError
	if !errors.As(err, &dup) {
		t.Fatalf("err = %#v, want *DuplicateRegistrationError", err)
	}
	if dup.Kind != "resource" || dup.Key != "test://x/1" {
		t.Fatalf("err = %+v, want kind=resource key=test://x/1", dup)
	}
	if got := s.RegisteredResourceURIs(); !slices.Equal(got, []string{"test://x/1"}) {
		t.Fatalf("RegisteredResourceURIs() = %v, want [test://x/1]", got)
	}
}

func TestAddResourceTemplateRejectsDuplicate(t *testing.T) {
	s := New("test", "v0")
	tmpl := &mcp.ResourceTemplate{URITemplate: "test://tenants/{tenant}/x", Name: "t", MIMEType: "text/plain"}
	if err := s.AddResourceTemplate(tmpl, testResourceHandler); err != nil {
		t.Fatalf("AddResourceTemplate: %v", err)
	}

	var dup *DuplicateRegistrationError
	if err := s.AddResourceTemplate(tmpl, testResourceHandler); !errors.As(err, &dup) {
		t.Fatalf("err = %#v, want *DuplicateRegistrationError", err)
	}
	if got := s.RegisteredResourceTemplates(); !slices.Equal(got, []string{"test://tenants/{tenant}/x"}) {
		t.Fatalf("RegisteredResourceTemplates() = %v, want one entry", got)
	}
}

// TestRegistriesAreIndependent verifies a tool and a resource may share a
// key without colliding — the SDK keeps separate registries per primitive.
func TestRegistriesAreIndependent(t *testing.T) {
	s := New("test", "v0")
	mustAdd(t, s, "shared_key", "tool")
	if err := s.AddResource(testResource("shared_key"), testResourceHandler); err != nil {
		t.Fatalf("unexpected cross-primitive collision: %v", err)
	}
}

// TestNotifyResourceListChangedDoesNotClaimKeys verifies the internal
// add/remove sentinel stays out of the registry, so it can fire any
// number of times.
func TestNotifyResourceListChangedDoesNotClaimKeys(t *testing.T) {
	s := New("test", "v0")
	s.NotifyResourceListChanged()
	s.NotifyResourceListChanged()

	if got := s.RegisteredResourceTemplates(); len(got) != 0 {
		t.Fatalf("RegisteredResourceTemplates() = %v, want empty", got)
	}
}

// mustAdd registers a marker tool, failing the test on error.
func mustAdd(t *testing.T, s *Server, name, marker string) {
	t.Helper()
	if err := AddTool(s, testTool(name), testToolHandler(marker)); err != nil {
		t.Fatalf("AddTool(%q): %v", name, err)
	}
}

// TestServerAddToolClaimsSameRegistryAsPackageAddTool verifies the
// non-generic method and the generic function share one name space: a
// hand-written tool cannot quietly displace a generated one.
func TestServerAddToolClaimsSameRegistryAsPackageAddTool(t *testing.T) {
	s := New("test", "v0")
	mustAdd(t, s, "shared_name", "generic")

	err := s.AddTool(testTool("shared_name"), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, nil
	})
	if err == nil {
		t.Fatalf("expected an error: the generic AddTool already claimed the name")
	}
	if got := s.RegisteredToolNames(); !slices.Equal(got, []string{"shared_name"}) {
		t.Fatalf("RegisteredToolNames() = %v, want [shared_name]", got)
	}
}

// TestServerAddToolIsSnapshotted verifies plain handlers are cataloged
// like generic ones.
func TestServerAddToolIsSnapshotted(t *testing.T) {
	s := New("test", "v0")
	if err := s.AddTool(testTool("plain_tool"), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("AddTool: %v", err)
	}
	if got := s.RegisteredToolNames(); !slices.Equal(got, []string{"plain_tool"}) {
		t.Fatalf("RegisteredToolNames() = %v, want [plain_tool]", got)
	}
}
