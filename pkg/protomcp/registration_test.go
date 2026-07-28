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

func testPlainToolHandler(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{}, nil
}

// recoverPanic runs fn and returns the value it panicked with, or nil.
func recoverPanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// recoverDuplicate runs fn and returns the *DuplicateRegistrationError it
// panicked with, failing the test if it did not panic with one.
func recoverDuplicate(t *testing.T, fn func()) *DuplicateRegistrationError {
	t.Helper()
	recovered := recoverPanic(fn)
	if recovered == nil {
		t.Fatalf("expected a panic on duplicate registration")
	}
	err, ok := recovered.(error)
	if !ok {
		t.Fatalf("panic value = %#v, want error", recovered)
	}
	var duplicate *DuplicateRegistrationError
	if !errors.As(err, &duplicate) {
		t.Fatalf("panic value %#v is not a *DuplicateRegistrationError", err)
	}
	return duplicate
}

// TestMustAddToolClaimsName verifies distinct names register cleanly and
// are reported in sorted order.
func TestMustAddToolClaimsName(t *testing.T) {
	s := New("test", "v0")
	MustAddTool(s, testTool("b_tool"), testToolHandler("b"))
	MustAddTool(s, testTool("a_tool"), testToolHandler("a"))

	got := s.RegisteredToolNames()
	if want := []string{"a_tool", "b_tool"}; !slices.Equal(got, want) {
		t.Fatalf("RegisteredToolNames() = %v, want %v", got, want)
	}
}

// TestMustAddToolPanicsOnDuplicateName is the core guarantee: the SDK
// would replace the first registration's handler silently, so protomcp
// refuses the second. The definitions here are byte-identical and only the
// handler differs — the shape generated registrars produce when the same
// registrar is invoked twice with different clients, and the one no
// wire-visible catalog diff can detect.
//
// The panic carries the error value itself, so a caller that recovers can
// errors.As it rather than matching on message text.
func TestMustAddToolPanicsOnDuplicateName(t *testing.T) {
	s := New("test", "v0")
	MustAddTool(s, testTool("dup_tool"), testToolHandler("first"))

	duplicate := recoverDuplicate(t, func() {
		MustAddTool(s, testTool("dup_tool"), testToolHandler("second"))
	})
	if duplicate.Kind != "tool" || duplicate.Key != "dup_tool" {
		t.Fatalf("err = %+v, want kind=tool key=dup_tool", duplicate)
	}
	if !strings.Contains(duplicate.Error(), "dup_tool") {
		t.Fatalf("error %q does not name the offending tool", duplicate)
	}

	// The first registration is still the only one recorded.
	if got := s.RegisteredToolNames(); !slices.Equal(got, []string{"dup_tool"}) {
		t.Fatalf("RegisteredToolNames() = %v, want [dup_tool]", got)
	}
}

// TestMustAddToolPanicsOnEmptyName guards the registry key itself: an
// unnamed tool is unaddressable and would collide with the next unnamed one.
func TestMustAddToolPanicsOnEmptyName(t *testing.T) {
	s := New("test", "v0")
	if recoverPanic(func() { MustAddTool(s, testTool(""), testToolHandler("x")) }) == nil {
		t.Fatalf("expected a panic on an empty tool name")
	}
}

func TestMustAddToolPanicsOnNilTool(t *testing.T) {
	s := New("test", "v0")
	if recoverPanic(func() { MustAddTool(s, nil, testToolHandler("x")) }) == nil {
		t.Fatalf("expected a panic on a nil tool")
	}
}

// TestSnapshotDiffYieldsBatchRegistrations verifies the contract callers
// rely on to bind primitives to a scope: keys are only ever added, so the
// difference of two snapshots is exactly what the batch registered.
func TestSnapshotDiffYieldsBatchRegistrations(t *testing.T) {
	s := New("test", "v0")
	MustAddTool(s, testTool("existing_tool"), testToolHandler("existing"))

	before := s.RegisteredToolNames()
	MustAddTool(s, testTool("batch_tool_1"), testToolHandler("1"))
	MustAddTool(s, testTool("batch_tool_2"), testToolHandler("2"))
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
	if recovered := recoverPanic(func() {
		MustAddTool(s, testTool("bypass_tool"), testToolHandler("claimed"))
	}); recovered != nil {
		t.Fatalf("unexpected panic claiming a name the SDK already holds: %v", recovered)
	}
}

func TestMustAddResourcePanicsOnDuplicateURI(t *testing.T) {
	s := New("test", "v0")
	s.MustAddResource(testResource("test://x/1"), testResourceHandler)

	duplicate := recoverDuplicate(t, func() {
		s.MustAddResource(testResource("test://x/1"), testResourceHandler)
	})
	if duplicate.Kind != "resource" || duplicate.Key != "test://x/1" {
		t.Fatalf("err = %+v, want kind=resource key=test://x/1", duplicate)
	}
	if got := s.RegisteredResourceURIs(); !slices.Equal(got, []string{"test://x/1"}) {
		t.Fatalf("RegisteredResourceURIs() = %v, want [test://x/1]", got)
	}
}

func TestMustAddResourceTemplatePanicsOnDuplicate(t *testing.T) {
	s := New("test", "v0")
	tmpl := &mcp.ResourceTemplate{URITemplate: "test://tenants/{tenant}/x", Name: "t", MIMEType: "text/plain"}
	s.MustAddResourceTemplate(tmpl, testResourceHandler)

	duplicate := recoverDuplicate(t, func() { s.MustAddResourceTemplate(tmpl, testResourceHandler) })
	if duplicate.Kind != "resource template" {
		t.Fatalf("err = %+v, want kind=resource template", duplicate)
	}
	if got := s.RegisteredResourceTemplates(); !slices.Equal(got, []string{"test://tenants/{tenant}/x"}) {
		t.Fatalf("RegisteredResourceTemplates() = %v, want one entry", got)
	}
}

// TestRegistriesAreIndependent verifies a tool and a resource may share a
// key without colliding — the SDK keeps separate registries per primitive.
func TestRegistriesAreIndependent(t *testing.T) {
	s := New("test", "v0")
	MustAddTool(s, testTool("shared_key"), testToolHandler("tool"))
	if recovered := recoverPanic(func() {
		s.MustAddResource(testResource("shared_key"), testResourceHandler)
	}); recovered != nil {
		t.Fatalf("unexpected cross-primitive collision: %v", recovered)
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

// TestServerMustAddToolSharesRegistryWithPackageForm verifies the
// non-generic method and the generic function share one name space: a
// hand-written tool cannot quietly displace a generated one.
func TestServerMustAddToolSharesRegistryWithPackageForm(t *testing.T) {
	s := New("test", "v0")
	MustAddTool(s, testTool("shared_name"), testToolHandler("generic"))

	duplicate := recoverDuplicate(t, func() {
		s.MustAddTool(testTool("shared_name"), testPlainToolHandler)
	})
	if duplicate.Key != "shared_name" {
		t.Fatalf("duplicate.Key = %q, want shared_name", duplicate.Key)
	}
	if got := s.RegisteredToolNames(); !slices.Equal(got, []string{"shared_name"}) {
		t.Fatalf("RegisteredToolNames() = %v, want [shared_name]", got)
	}
}

// TestServerMustAddToolIsSnapshotted verifies plain handlers are cataloged
// like generic ones.
func TestServerMustAddToolIsSnapshotted(t *testing.T) {
	s := New("test", "v0")
	s.MustAddTool(testTool("plain_tool"), testPlainToolHandler)
	if got := s.RegisteredToolNames(); !slices.Equal(got, []string{"plain_tool"}) {
		t.Fatalf("RegisteredToolNames() = %v, want [plain_tool]", got)
	}
}
