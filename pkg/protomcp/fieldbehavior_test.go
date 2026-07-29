// External test package so we can import the example proto fixture
// (which itself depends on protomcp) without an import cycle.
package protomcp_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	authv1 "github.com/akuity/protomcp/pkg/api/gen/examples/auth/v1"
	greeterv1 "github.com/akuity/protomcp/pkg/api/gen/examples/greeter/v1"
	"github.com/akuity/protomcp/pkg/protomcp"
)

// TestClearOutputOnly_TopLevelScalar, the canonical case: a message
// whose only fields are OUTPUT_ONLY scalars. Every field must be zeroed.
func TestClearOutputOnly_TopLevelScalar(t *testing.T) {
	m := &authv1.WhoAmIResponse{UserId: "alice", Tenant: "acme"}
	protomcp.ClearOutputOnly(m)
	if m.UserId != "" {
		t.Errorf("UserId = %q, want empty", m.UserId)
	}
	if m.Tenant != "" {
		t.Errorf("Tenant = %q, want empty", m.Tenant)
	}
}

// TestClearOutputOnly_NestedMessage, OUTPUT_ONLY inside a nested
// message field. This is the bug the original review was worried
// about: without recursion, the inner field leaks through.
func TestClearOutputOnly_NestedMessage(t *testing.T) {
	m := &authv1.TestNested{
		Inner: &authv1.TestInner{
			ServerId: "admin", // OUTPUT_ONLY, must be cleared
			UserName: "alice", // regular, must survive
		},
	}
	protomcp.ClearOutputOnly(m)
	if m.Inner == nil {
		t.Fatalf("Inner was cleared entirely; only ServerId should be")
	}
	if m.Inner.ServerId != "" {
		t.Errorf("Inner.ServerId = %q, want empty (nested OUTPUT_ONLY leaked)", m.Inner.ServerId)
	}
	if m.Inner.UserName != "alice" {
		t.Errorf("Inner.UserName = %q, want %q (regular field wiped)", m.Inner.UserName, "alice")
	}
}

// TestClearOutputOnly_OutputOnlyMessageField, when the MESSAGE field
// itself is OUTPUT_ONLY, the whole nested value is cleared. No recursion
// needed (or performed).
func TestClearOutputOnly_OutputOnlyMessageField(t *testing.T) {
	m := &authv1.TestOutputOnlyMessage{
		Stripped: &authv1.TestInner{ServerId: "x", UserName: "y"},
		Kept:     "surface",
	}
	protomcp.ClearOutputOnly(m)
	if m.Stripped != nil {
		t.Errorf("Stripped = %+v, want nil", m.Stripped)
	}
	if m.Kept != "surface" {
		t.Errorf("Kept = %q, want %q", m.Kept, "surface")
	}
}

// TestClearOutputOnly_RepeatedMessages, recursive clearing must reach
// every element of a repeated<Message> field.
func TestClearOutputOnly_RepeatedMessages(t *testing.T) {
	m := &authv1.TestRepeatedMessages{
		Items: []*authv1.TestInner{
			{ServerId: "s1", UserName: "u1"},
			{ServerId: "s2", UserName: "u2"},
			{ServerId: "s3", UserName: "u3"},
		},
	}
	protomcp.ClearOutputOnly(m)
	if len(m.Items) != 3 {
		t.Fatalf("Items length = %d, want 3 (list itself should not be cleared)", len(m.Items))
	}
	for i, it := range m.Items {
		if it.ServerId != "" {
			t.Errorf("Items[%d].ServerId = %q, want empty", i, it.ServerId)
		}
		if it.UserName == "" {
			t.Errorf("Items[%d].UserName was wiped", i)
		}
	}
}

// TestClearOutputOnly_MapMessages, each message-valued entry in a
// map must be recursively cleared; the map keys and structure survive.
func TestClearOutputOnly_MapMessages(t *testing.T) {
	m := &authv1.TestMapMessages{
		Items: map[string]*authv1.TestInner{
			"alpha": {ServerId: "sa", UserName: "a"},
			"beta":  {ServerId: "sb", UserName: "b"},
		},
	}
	protomcp.ClearOutputOnly(m)
	if len(m.Items) != 2 {
		t.Fatalf("Items length = %d, want 2", len(m.Items))
	}
	for k, v := range m.Items {
		if v.ServerId != "" {
			t.Errorf("Items[%q].ServerId = %q, want empty", k, v.ServerId)
		}
		if v.UserName == "" {
			t.Errorf("Items[%q].UserName was wiped", k)
		}
	}
}

// TestClearOutputOnly_OneofOutputOnlySelected, if the OUTPUT_ONLY
// member of a oneof is the currently-selected one, the oneof must be
// cleared. Has() on the oneof reports false afterwards.
func TestClearOutputOnly_OneofOutputOnlySelected(t *testing.T) {
	m := &authv1.TestOneofOutputOnly{
		Choice: &authv1.TestOneofOutputOnly_Computed{Computed: "server-picked"},
	}
	protomcp.ClearOutputOnly(m)
	if m.Choice != nil {
		t.Errorf("Choice = %+v, want nil after OUTPUT_ONLY oneof member cleared", m.Choice)
	}
}

// TestClearOutputOnly_OneofRegularSelected, when a non-OUTPUT_ONLY
// oneof member is selected, Clear on the OUTPUT_ONLY sibling is a
// no-op. The selected member must survive.
func TestClearOutputOnly_OneofRegularSelected(t *testing.T) {
	m := &authv1.TestOneofOutputOnly{
		Choice: &authv1.TestOneofOutputOnly_Manual{Manual: "user-picked"},
	}
	protomcp.ClearOutputOnly(m)
	manual, ok := m.Choice.(*authv1.TestOneofOutputOnly_Manual)
	if !ok {
		t.Fatalf("Choice = %T, want *TestOneofOutputOnly_Manual", m.Choice)
	}
	if manual.Manual != "user-picked" {
		t.Errorf("Manual = %q, want %q", manual.Manual, "user-picked")
	}
}

// TestClearOutputOnly_RepeatedScalarOutputOnly, OUTPUT_ONLY on a
// repeated scalar field clears the entire list.
func TestClearOutputOnly_RepeatedScalarOutputOnly(t *testing.T) {
	m := &authv1.TestRepeatedOutputOnlyScalar{
		ServerIds: []string{"sid-1", "sid-2"},
		Names:     []string{"alice", "bob"},
	}
	protomcp.ClearOutputOnly(m)
	if len(m.ServerIds) != 0 {
		t.Errorf("ServerIds = %v, want empty", m.ServerIds)
	}
	if len(m.Names) != 2 {
		t.Errorf("Names = %v, want length 2 (non-OUTPUT_ONLY list wiped)", m.Names)
	}
}

// TestClearOutputOnly_NoLeafAnnotation, recursing into a message
// type with no OUTPUT_ONLY fields anywhere must be a no-op but not
// panic. WhoAmIRequest is empty; TestInner has a user field that
// must survive when called directly.
func TestClearOutputOnly_NoChangeForRegularFields(t *testing.T) {
	m := &authv1.TestInner{UserName: "alice"}
	protomcp.ClearOutputOnly(m)
	if m.UserName != "alice" {
		t.Errorf("UserName = %q, want alice (nothing should have changed)", m.UserName)
	}
}

// TestClearOutputOnly_NilSafe, generated code should never pass nil,
// but the helper is defensive.
func TestClearOutputOnly_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ClearOutputOnly(nil) panicked: %v", r)
		}
	}()
	protomcp.ClearOutputOnly(nil)
}

// TestClearOutputOnly_UnsetNestedNotRecursed, if a nested message
// field is unset (nil), we must NOT try to recurse into it (would
// panic on a nil reflect.Message). Has() gate protects us.
func TestClearOutputOnly_UnsetNestedNotRecursed(t *testing.T) {
	m := &authv1.TestNested{} // Inner is nil
	protomcp.ClearOutputOnly(m)
	if m.Inner != nil {
		t.Errorf("Inner = %+v, want nil (we shouldn't materialize it)", m.Inner)
	}
}

// TestClearOutputOnly_RecursionDepthCap, google.protobuf.Value is a
// recursive proto (Value ↔ Struct). protojson.Unmarshal of a crafted
// payload can produce a tree arbitrarily deep, and an unbounded walk
// would blow the goroutine stack. The depth guard in
// clearOutputOnlyReflect must terminate cleanly well before that.
func TestClearOutputOnly_RecursionDepthCap(t *testing.T) {
	// Build 500 levels of Value → Struct → Value → …, five times the
	// cap so this test also pins the cap itself loosely (any cap ≪ 500
	// is acceptable; we only care that it stops).
	v := structpb.NewStringValue("leaf")
	for range 500 {
		v = structpb.NewStructValue(&structpb.Struct{
			Fields: map[string]*structpb.Value{"f": v},
		})
	}
	// Must not stack-overflow or panic.
	protomcp.ClearOutputOnly(v)
}

// TestClearOutputOnly_EmptyRepeatedNotRecursed, empty repeated/map
// must be skipped cleanly without iterating.
func TestClearOutputOnly_EmptyCollectionsSkipped(t *testing.T) {
	rm := &authv1.TestRepeatedMessages{}
	mm := &authv1.TestMapMessages{}
	protomcp.ClearOutputOnly(rm)
	protomcp.ClearOutputOnly(mm)
	if len(rm.Items) != 0 {
		t.Errorf("Repeated items mutated: %v", rm.Items)
	}
	if len(mm.Items) != 0 {
		t.Errorf("Map items mutated: %v", mm.Items)
	}
}

func TestClearOutputOnly_NilListAndMapElements(t *testing.T) {
	m := &authv1.TestRepeatedMessages{
		Items: []*authv1.TestInner{nil, {ServerId: "s", UserName: "u"}},
	}
	protomcp.ClearOutputOnly(m)
	if m.Items[1].ServerId != "" {
		t.Errorf("Items[1].ServerId = %q, want empty", m.Items[1].ServerId)
	}
	if m.Items[1].UserName != "u" {
		t.Errorf("Items[1].UserName = %q, want %q", m.Items[1].UserName, "u")
	}

	mm := &authv1.TestMapMessages{
		Items: map[string]*authv1.TestInner{"a": nil, "b": {ServerId: "s", UserName: "u"}},
	}
	protomcp.ClearOutputOnly(mm)
	if mm.Items["b"].ServerId != "" {
		t.Errorf(`Items["b"].ServerId = %q, want empty`, mm.Items["b"].ServerId)
	}

	holder := &authv1.TestAnyHolder{Items: []*anypb.Any{nil}}
	protomcp.ClearOutputOnly(holder)
}

func TestClearOutputOnly_AnyPayload(t *testing.T) {
	packed, err := anypb.New(&authv1.TestInner{ServerId: "admin", UserName: "alice"})
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	m := &authv1.TestAnyHolder{Payload: packed}
	protomcp.ClearOutputOnly(m)
	var got authv1.TestInner
	if err := m.Payload.UnmarshalTo(&got); err != nil {
		t.Fatalf("UnmarshalTo: %v", err)
	}
	if got.ServerId != "" {
		t.Errorf("packed ServerId = %q, want empty (OUTPUT_ONLY leaked through Any)", got.ServerId)
	}
	if got.UserName != "alice" {
		t.Errorf("packed UserName = %q, want alice", got.UserName)
	}
}

func TestClearSchemaExcluded_AnyPayload(t *testing.T) {
	packed, err := anypb.New(&greeterv1.EchoComplexRequest{Name: "n", InternalNote: "secret"})
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	m := &authv1.TestAnyHolder{Payload: packed}
	protomcp.ClearSchemaExcluded(m)
	var got greeterv1.EchoComplexRequest
	if err := m.Payload.UnmarshalTo(&got); err != nil {
		t.Fatalf("UnmarshalTo: %v", err)
	}
	if got.GetInternalNote() != "" {
		t.Errorf("packed InternalNote = %q, want empty (excluded field leaked through Any)", got.GetInternalNote())
	}
	if got.GetName() != "n" {
		t.Errorf("packed Name = %q, want n", got.GetName())
	}
}

func TestClearSchemaExcluded_AnyInListAndMap(t *testing.T) {
	mk := func() *anypb.Any {
		packed, err := anypb.New(&greeterv1.EchoComplexRequest{Name: "n", InternalNote: "secret"})
		if err != nil {
			t.Fatalf("anypb.New: %v", err)
		}
		return packed
	}
	m := &authv1.TestAnyHolder{
		Items: []*anypb.Any{mk()},
		ByKey: map[string]*anypb.Any{"k": mk()},
	}
	protomcp.ClearSchemaExcluded(m)
	for name, packed := range map[string]*anypb.Any{"Items[0]": m.Items[0], `ByKey["k"]`: m.ByKey["k"]} {
		var got greeterv1.EchoComplexRequest
		if err := packed.UnmarshalTo(&got); err != nil {
			t.Fatalf("%s UnmarshalTo: %v", name, err)
		}
		if got.GetInternalNote() != "" {
			t.Errorf("%s InternalNote = %q, want empty", name, got.GetInternalNote())
		}
		if got.GetName() != "n" {
			t.Errorf("%s Name = %q, want n", name, got.GetName())
		}
	}
}

func TestClearOutputOnly_AnyUnresolvableTypeCleared(t *testing.T) {
	m := &authv1.TestAnyHolder{
		Payload: &anypb.Any{TypeUrl: "type.googleapis.com/no.such.Type", Value: []byte{0x0a, 0x01, 0x78}},
	}
	protomcp.ClearOutputOnly(m)
	if m.Payload.GetTypeUrl() != "" || len(m.Payload.GetValue()) != 0 {
		t.Errorf("unresolvable Any not cleared: %+v", m.Payload)
	}
}

func TestClearOutputOnly_AnyUntouchedWhenNoMatches(t *testing.T) {
	packed, err := anypb.New(&greeterv1.HelloReply{Message: "hi"})
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	origValue := append([]byte(nil), packed.GetValue()...)
	origURL := packed.GetTypeUrl()
	m := &authv1.TestAnyHolder{Payload: packed}
	protomcp.ClearOutputOnly(m)
	if m.Payload.GetTypeUrl() != origURL || !bytes.Equal(m.Payload.GetValue(), origValue) {
		t.Errorf("Any with no matchable fields was rewritten: %+v", m.Payload)
	}
}

func TestClearOutputOnly_AnyPreservesBytesWhenMatchingFieldsAreUnset(t *testing.T) {
	payload := &authv1.TestMapMessages{Items: map[string]*authv1.TestInner{
		"a": {UserName: "one"},
		"b": {UserName: "two"},
		"c": {UserName: "three"},
		"d": {UserName: "four"},
		"e": {UserName: "five"},
		"f": {UserName: "six"},
		"g": {UserName: "seven"},
		"h": {UserName: "eight"},
	}}
	packed, err := anypb.New(payload)
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	original := bytes.Clone(packed.GetValue())
	for i := range 100 {
		m := &authv1.TestAnyHolder{Payload: proto.Clone(packed).(*anypb.Any)}
		protomcp.ClearOutputOnly(m)
		if !bytes.Equal(m.Payload.GetValue(), original) {
			t.Fatalf("iteration %d rewrote Any.Value even though no populated field was cleared", i)
		}
	}
}

func TestClearOutputOnly_AnyChangedPayloadUsesDeterministicEncoding(t *testing.T) {
	payload := &authv1.TestMapMessages{Items: map[string]*authv1.TestInner{
		"a": {ServerId: "1", UserName: "one"},
		"b": {ServerId: "2", UserName: "two"},
		"c": {ServerId: "3", UserName: "three"},
		"d": {ServerId: "4", UserName: "four"},
		"e": {ServerId: "5", UserName: "five"},
		"f": {ServerId: "6", UserName: "six"},
		"g": {ServerId: "7", UserName: "seven"},
		"h": {ServerId: "8", UserName: "eight"},
	}}
	packed, err := anypb.New(payload)
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	encodings := map[string]struct{}{}
	for range 100 {
		m := &authv1.TestAnyHolder{Payload: proto.Clone(packed).(*anypb.Any)}
		protomcp.ClearOutputOnly(m)
		encodings[string(m.Payload.GetValue())] = struct{}{}
	}
	if len(encodings) != 1 {
		t.Fatalf("changed Any payload produced %d wire encodings, want 1", len(encodings))
	}
}

// newDynamicAnyPayload builds a message type that exists only in the
// returned local registry (never in protoregistry.GlobalTypes) and an
// Any packing an instance of it, reproducing a caller that configures a
// custom protojson Resolver for dynamically loaded types.
func newDynamicAnyPayload(t *testing.T) (*protoregistry.Types, *anypb.Any) {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("protomcp/dynamictest/payload.proto"),
		Package: proto.String("protomcp.dynamictest"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Payload"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:     proto.String("value"),
				Number:   proto.Int32(1),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				JsonName: proto.String("value"),
			}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	mt := dynamicpb.NewMessageType(fd.Messages().Get(0))
	types := &protoregistry.Types{}
	if rErr := types.RegisterMessage(mt); rErr != nil {
		t.Fatalf("RegisterMessage: %v", rErr)
	}
	payload := mt.New()
	payload.Set(mt.Descriptor().Fields().ByName("value"), protoreflect.ValueOfString("kept"))
	raw, err := proto.Marshal(payload.Interface())
	if err != nil {
		t.Fatalf("marshal dynamic payload: %v", err)
	}
	return types, &anypb.Any{
		TypeUrl: "type.googleapis.com/protomcp.dynamictest.Payload",
		Value:   raw,
	}
}

func TestServerClear_AnyUsesConfiguredResolver(t *testing.T) {
	types, packed := newDynamicAnyPayload(t)
	srv := protomcp.New("t", "0.0.1",
		protomcp.WithProtoJSONUnmarshal(protojson.UnmarshalOptions{Resolver: types}))

	origURL := packed.GetTypeUrl()
	origValue := append([]byte(nil), packed.GetValue()...)
	m := &authv1.TestAnyHolder{Payload: packed}
	srv.ClearOutputOnly(m)
	srv.ClearSchemaExcluded(m)
	if m.Payload.GetTypeUrl() != origURL || !bytes.Equal(m.Payload.GetValue(), origValue) {
		t.Errorf("Any resolvable via the configured resolver was cleared: %+v", m.Payload)
	}

	protomcp.ClearOutputOnly(m)
	if m.Payload.GetTypeUrl() != "" || len(m.Payload.GetValue()) != 0 {
		t.Errorf("package-level clear must stay conservative for types outside GlobalTypes: %+v", m.Payload)
	}
}

// buildNodeChain returns a TestNode chain with links+1 nodes, every one
// carrying an excluded secret.
func buildNodeChain(links int) *authv1.TestNode {
	head := &authv1.TestNode{Secret: "deep-secret", Data: "d"}
	cur := head
	for range links {
		cur.Next = &authv1.TestNode{Secret: "deep-secret", Data: "d"}
		cur = cur.Next
	}
	return head
}

func TestClearSchemaExcluded_DeepChainFailsClosed(t *testing.T) {
	head := buildNodeChain(10050)
	protomcp.ClearSchemaExcluded(head)
	depth := 0
	for n := head; n != nil; n = n.Next {
		if n.GetSecret() != "" {
			t.Fatalf("node at depth %d kept its excluded secret %q; deep nesting must fail closed", depth, n.GetSecret())
		}
		depth++
	}
	if head.GetData() != "d" {
		t.Errorf("head.Data = %q, want untouched %q", head.GetData(), "d")
	}
}

// TestBoolPtr, basic coverage of the helper used by the generator.
func TestBoolPtr(t *testing.T) {
	if p := protomcp.BoolPtr(true); p == nil || *p != true {
		t.Errorf("BoolPtr(true) = %v", p)
	}
	if p := protomcp.BoolPtr(false); p == nil || *p != false {
		t.Errorf("BoolPtr(false) = %v", p)
	}
}
