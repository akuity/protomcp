package protomcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/akuity/protomcp/pkg/api/gen/examples/auth/v1"
	greeterv1 "github.com/akuity/protomcp/pkg/api/gen/examples/greeter/v1"
	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
	"github.com/akuity/protomcp/pkg/protomcp"
)

func TestClearSchemaExcluded(t *testing.T) {
	msg := &greeterv1.EchoComplexRequest{
		Name:         "n",
		Tags:         []string{"a"},
		InternalNote: "secret",
	}
	protomcp.ClearSchemaExcluded(msg)
	if msg.GetInternalNote() != "" {
		t.Errorf("InternalNote = %q, want cleared", msg.GetInternalNote())
	}
	if msg.GetName() != "n" || len(msg.GetTags()) != 1 {
		t.Errorf("unexcluded fields were modified: %+v", msg)
	}

	resp := &greeterv1.EchoComplexResponse{Name: "n", InternalNote: "secret"}
	protomcp.ClearSchemaExcluded(resp)
	if resp.GetInternalNote() != "" {
		t.Errorf("response InternalNote = %q, want cleared", resp.GetInternalNote())
	}

	protomcp.ClearSchemaExcluded(nil)
	protomcp.ClearSchemaExcluded((*greeterv1.EchoComplexRequest)(nil))
}

func TestMarshalProtoMaskedStripsExcludedFieldNames(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	payload, err := srv.MarshalProtoMasked(&greeterv1.EchoComplexResponse{
		Name:         "n",
		InternalNote: "secret",
		Address:      &greeterv1.Address{City: "c"},
	})
	if err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, leaked := decoded["internalNote"]; leaked {
		t.Errorf("internalNote key leaked into masked JSON: %s", payload)
	}
	if decoded["name"] != "n" {
		t.Errorf("name = %v, want n", decoded["name"])
	}
	if _, ok := decoded["address"]; !ok {
		t.Errorf("unexcluded message field dropped: %s", payload)
	}
	if _, ok := decoded["tags"]; !ok {
		t.Errorf("EmitDefaultValues zero fields must survive masking: %s", payload)
	}
}

func TestMarshalProtoMaskedInvalidUTF8InExcludedField(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	payload, err := srv.MarshalProtoMasked(&greeterv1.EchoComplexResponse{
		Name:         "n",
		InternalNote: "\xff\xfe",
	})
	if err != nil {
		t.Fatalf("MarshalProtoMasked with invalid UTF-8 in excluded field: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, leaked := decoded["internalNote"]; leaked {
		t.Errorf("internalNote key leaked into masked JSON: %s", payload)
	}
	if decoded["name"] != "n" {
		t.Errorf("name = %v, want n", decoded["name"])
	}
}

func TestMarshalProtoMaskedAnyPayload(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	packed, err := anypb.New(&greeterv1.EchoComplexResponse{Name: "n", InternalNote: "secret"})
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	holder := &authv1.TestAnyHolder{Payload: packed}
	payload, err := srv.MarshalProtoMasked(holder)
	if err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	if strings.Contains(string(payload), "secret") {
		t.Errorf("excluded value leaked through Any: %s", payload)
	}
	if strings.Contains(string(payload), "internalNote") {
		t.Errorf("excluded field name leaked through Any: %s", payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	inner, ok := decoded["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing or not an object: %s", payload)
	}
	if _, ok := inner["@type"].(string); !ok {
		t.Errorf("@type missing from Any JSON: %s", payload)
	}
	if inner["name"] != "n" {
		t.Errorf("packed name = %v, want n", inner["name"])
	}
}

func TestDefaultToolErrorHandlerMasksExcludedDetailFields(t *testing.T) {
	st, err := status.New(codes.InvalidArgument, "bad").WithDetails(
		&greeterv1.EchoComplexResponse{Name: "n", InternalNote: "server-secret"},
	)
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	res, hErr := protomcp.DefaultToolErrorHandler(context.Background(), &mcp.CallToolRequest{}, st.Err())
	if hErr != nil {
		t.Fatalf("DefaultToolErrorHandler: %v", hErr)
	}
	raw, ok := res.StructuredContent.(json.RawMessage)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want json.RawMessage", res.StructuredContent)
	}
	if strings.Contains(string(raw), "server-secret") {
		t.Errorf("excluded value leaked via error details: %s", raw)
	}
	var decoded struct {
		Details []map[string]any `json:"details"`
	}
	if uErr := json.Unmarshal(raw, &decoded); uErr != nil {
		t.Fatalf("StructuredContent not valid JSON: %v", uErr)
	}
	if len(decoded.Details) != 1 {
		t.Fatalf("details = %v, want 1 entry", decoded.Details)
	}
	if _, leaked := decoded.Details[0]["internalNote"]; leaked {
		t.Errorf("excluded field name leaked via error details: %s", raw)
	}
	if decoded.Details[0]["name"] != "n" {
		t.Errorf("unexcluded detail field dropped: %s", raw)
	}
	var original greeterv1.EchoComplexResponse
	if uErr := st.Proto().GetDetails()[0].UnmarshalTo(&original); uErr != nil {
		t.Fatalf("UnmarshalTo: %v", uErr)
	}
	if original.GetInternalNote() != "server-secret" {
		t.Errorf("handler mutated the caller's status; InternalNote = %q", original.GetInternalNote())
	}
}

func TestMarshalProtoMaskedDeepChainErrors(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	if _, err := srv.MarshalProtoMasked(buildNodeChain(10050)); err == nil {
		t.Fatal("MarshalProtoMasked accepted nesting beyond the masking depth bound; output masking must fail loud")
	}
}

func TestMarshalProtoMaskedCorruptAnyErrors(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	url := "type.googleapis.com/" + string((&greeterv1.EchoComplexResponse{}).ProtoReflect().Descriptor().FullName())
	holder := &authv1.TestAnyHolder{Payload: &anypb.Any{TypeUrl: url, Value: []byte{0xff}}}
	if _, err := srv.MarshalProtoMasked(holder); err == nil {
		t.Fatal("MarshalProtoMasked serialized a corrupt Any payload as success")
	}
	protomcp.ClearSchemaExcluded(holder)
	if holder.GetPayload().GetTypeUrl() != "" || len(holder.GetPayload().GetValue()) != 0 {
		t.Errorf("input-side clear must stay fail-closed for corrupt Any: %+v", holder.GetPayload())
	}
}

func TestMarshalProtoMaskedUnresolvableAnyErrors(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	holder := &authv1.TestAnyHolder{
		Payload: &anypb.Any{TypeUrl: "type.googleapis.com/no.such.Type", Value: []byte{0x0a, 0x01, 0x78}},
	}
	if _, err := srv.MarshalProtoMasked(holder); err == nil {
		t.Fatal("MarshalProtoMasked serialized an unresolvable Any payload as success")
	}
}

func TestDefaultToolErrorHandlerDeepDetailOmitsStructuredContent(t *testing.T) {
	st, err := status.New(codes.Internal, "boom").WithDetails(buildNodeChain(10050))
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	res, hErr := protomcp.DefaultToolErrorHandler(context.Background(), &mcp.CallToolRequest{}, st.Err())
	if hErr != nil {
		t.Fatalf("DefaultToolErrorHandler: %v", hErr)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError result, got %+v", res)
	}
	if res.StructuredContent != nil {
		t.Fatalf("StructuredContent must be omitted when details cannot be verifiably masked, got %s", res.StructuredContent)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || strings.Contains(tc.Text, "deep-secret") {
		t.Fatalf("text content leaked or wrong type: %+v", res.Content[0])
	}
}

// newFakeGoogleProtobufAny builds a message type that lives in the
// google.protobuf package but is NOT one of protojson's custom-format
// WKTs, with an excluded field, packed into an Any. Registered only in
// the returned local registry.
func newFakeGoogleProtobufAny(t *testing.T) (*protoregistry.Types, *anypb.Any) {
	t.Helper()
	secretOpts := &descriptorpb.FieldOptions{}
	proto.SetExtension(secretOpts, protomcpv1.E_FieldSchema, &protomcpv1.FieldSchemaOptions{Exclude: true})
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("protomcp/dynamictest/fake_wkt.proto"),
		Package: proto.String("google.protobuf"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("FakeThing"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:     proto.String("secret"),
					Number:   proto.Int32(1),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					JsonName: proto.String("secret"),
					Options:  secretOpts,
				},
				{
					Name:     proto.String("visible"),
					Number:   proto.Int32(2),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					JsonName: proto.String("visible"),
				},
			},
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
	msgFields := mt.Descriptor().Fields()
	payload.Set(msgFields.ByName("secret"), protoreflect.ValueOfString("hidden"))
	payload.Set(msgFields.ByName("visible"), protoreflect.ValueOfString("keep-me"))
	raw, err := proto.Marshal(payload.Interface())
	if err != nil {
		t.Fatalf("marshal fake payload: %v", err)
	}
	return types, &anypb.Any{
		TypeUrl: "type.googleapis.com/google.protobuf.FakeThing",
		Value:   raw,
	}
}

func TestMarshalProtoMaskedGoogleProtobufNonWKTIsStripped(t *testing.T) {
	types, packed := newFakeGoogleProtobufAny(t)
	srv := protomcp.New("t", "0.0.1",
		protomcp.WithProtoJSONMarshal(protojson.MarshalOptions{EmitDefaultValues: true, Resolver: types}))
	payload, err := srv.MarshalProtoMasked(&authv1.TestAnyHolder{Payload: packed})
	if err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	if strings.Contains(string(payload), "hidden") {
		t.Errorf("excluded value leaked through google.protobuf-package Any: %s", payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	inner, ok := decoded["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing or not an object: %s", payload)
	}
	if _, leaked := inner["secret"]; leaked {
		t.Errorf("excluded field name leaked; google.protobuf package prefix must not skip the strip pass: %s", payload)
	}
	if inner["visible"] != "keep-me" {
		t.Errorf("visible = %v, want keep-me: %s", inner["visible"], payload)
	}
}

func TestMarshalProtoMaskedWKTAnyValuePreserved(t *testing.T) {
	packed, err := anypb.New(timestamppb.New(time.Unix(1, 0)))
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	srv := protomcp.New("t", "0.0.1")
	payload, err := srv.MarshalProtoMasked(&authv1.TestAnyHolder{Payload: packed})
	if err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	inner, ok := decoded["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing or not an object: %s", payload)
	}
	if _, ok := inner["value"].(string); !ok {
		t.Errorf("Timestamp Any lost its custom-format value slot: %s", payload)
	}
}

func TestMarshalProtoMaskedAnyUsesConfiguredResolver(t *testing.T) {
	types, packed := newDynamicAnyPayload(t)
	srv := protomcp.New("t", "0.0.1",
		protomcp.WithProtoJSONMarshal(protojson.MarshalOptions{EmitDefaultValues: true, Resolver: types}))

	payload, err := srv.MarshalProtoMasked(&authv1.TestAnyHolder{Payload: packed})
	if err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	inner, ok := decoded["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing or not an object: %s", payload)
	}
	if _, ok := inner["@type"].(string); !ok {
		t.Errorf("@type dropped; Any resolvable via the configured resolver was cleared: %s", payload)
	}
	if inner["value"] != "kept" {
		t.Errorf("packed value = %v, want kept: %s", inner["value"], payload)
	}
}

func TestMarshalProtoMaskedPreservesProtojsonFormat(t *testing.T) {
	srv := protomcp.New("t", "0.0.1", protomcp.WithProtoJSONMarshal(protojson.MarshalOptions{}))
	payload, err := srv.MarshalProtoMasked(&greeterv1.EchoComplexResponse{
		Name:         "n",
		InternalNote: "secret",
		Address:      &greeterv1.Address{City: "c"},
	})
	if err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	if strings.Contains(string(payload), "internalNote") {
		t.Errorf("internalNote leaked: %s", payload)
	}
	iName := bytes.Index(payload, []byte(`"name"`))
	iAddr := bytes.Index(payload, []byte(`"address"`))
	if iName < 0 || iAddr < 0 || iName > iAddr {
		t.Errorf("expected protojson field order (name before address), got: %s", payload)
	}
}

func TestMarshalProtoMaskedMultilineIndent(t *testing.T) {
	srv := protomcp.New("t", "0.0.1", protomcp.WithProtoJSONMarshal(protojson.MarshalOptions{
		Multiline:         true,
		EmitDefaultValues: true,
	}))
	payload, err := srv.MarshalProtoMasked(&greeterv1.EchoComplexResponse{
		Name:         "n",
		InternalNote: "secret",
	})
	if err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	if strings.Contains(string(payload), "internalNote") {
		t.Errorf("internalNote leaked: %s", payload)
	}
	if !strings.Contains(string(payload), "\n  ") {
		t.Errorf("Multiline output lost its indentation: %q", payload)
	}
}

func TestMarshalProtoMaskedDoesNotMutateOriginal(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	msg := &greeterv1.EchoComplexResponse{
		Name:         "n",
		InternalNote: "secret",
		Address:      &greeterv1.Address{City: "c"},
	}
	if _, err := srv.MarshalProtoMasked(msg); err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	if msg.GetInternalNote() != "secret" {
		t.Errorf("InternalNote = %q, want %q: masking must not mutate the original message", msg.GetInternalNote(), "secret")
	}
	if msg.GetAddress().GetCity() != "c" {
		t.Errorf("Address.City = %q, want %q: masking must not mutate nested messages", msg.GetAddress().GetCity(), "c")
	}
}
