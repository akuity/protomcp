package protomcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	greeterv1 "github.com/akuity/protomcp/pkg/api/gen/examples/greeter/v1"
	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
	"github.com/akuity/protomcp/pkg/protomcp"
)

const (
	listRPC = "masktest.v1.Inventory.ListProducts"
	getRPC  = "masktest.v1.Inventory.GetProduct"
)

// newListReply builds a dynamic ListProductsReply with two products.
// Two fields opt out of listRPC's output: the top-level repeated `tags`
// and, inside every product, the repeated `notes`; `name` and `title`
// carry no annotation.
func newListReply(t *testing.T) proto.Message {
	t.Helper()
	excl := func() *descriptorpb.FieldOptions {
		o := &descriptorpb.FieldOptions{}
		proto.SetExtension(o, protomcpv1.E_FieldSchema,
			&protomcpv1.FieldSchemaOptions{ExcludeFromOutputs: []string{listRPC}})
		return o
	}
	str := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			Number:   proto.Int32(number),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			JsonName: proto.String(name),
		}
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("masktest/inventory.proto"),
		Package: proto.String("masktest.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Note"), Field: []*descriptorpb.FieldDescriptorProto{str("text", 1)}},
			{Name: proto.String("Tag"), Field: []*descriptorpb.FieldDescriptorProto{str("label", 1)}},
			{
				Name: proto.String("Product"),
				Field: []*descriptorpb.FieldDescriptorProto{
					str("name", 1),
					{
						Name:     proto.String("notes"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".masktest.v1.Note"),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						JsonName: proto.String("notes"),
						Options:  excl(),
					},
				},
			},
			{
				Name: proto.String("ListProductsReply"),
				Field: []*descriptorpb.FieldDescriptorProto{
					str("title", 1),
					{
						Name:     proto.String("products"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".masktest.v1.Product"),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						JsonName: proto.String("products"),
					},
					{
						Name:     proto.String("tags"),
						Number:   proto.Int32(3),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".masktest.v1.Tag"),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						JsonName: proto.String("tags"),
						Options:  excl(),
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	msgs := fd.Messages()
	product := func(name, note string) protoreflect.Value {
		p := dynamicpb.NewMessage(msgs.ByName("Product"))
		p.Set(p.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString(name))
		n := dynamicpb.NewMessage(msgs.ByName("Note"))
		n.Set(n.Descriptor().Fields().ByName("text"), protoreflect.ValueOfString(note))
		p.Mutable(p.Descriptor().Fields().ByName("notes")).List().Append(protoreflect.ValueOfMessage(n))
		return protoreflect.ValueOfMessage(p)
	}
	reply := dynamicpb.NewMessage(msgs.ByName("ListProductsReply"))
	fields := reply.Descriptor().Fields()
	reply.Set(fields.ByName("title"), protoreflect.ValueOfString("catalog"))
	products := reply.Mutable(fields.ByName("products")).List()
	products.Append(product("anvil", "heavy"))
	products.Append(product("rope", "long"))
	tag := dynamicpb.NewMessage(msgs.ByName("Tag"))
	tag.Set(tag.Descriptor().Fields().ByName("label"), protoreflect.ValueOfString("hardware"))
	reply.Mutable(fields.ByName("tags")).List().Append(protoreflect.ValueOfMessage(tag))
	return reply
}

func decodeFor(t *testing.T, srv *protomcp.Server, m proto.Message, rpc string) map[string]any {
	t.Helper()
	payload, err := srv.MarshalProtoMaskedForRPC(m, rpc)
	if err != nil {
		t.Fatalf("MarshalProtoMaskedForRPC(%q): %v", rpc, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", payload, err)
	}
	return decoded
}

// EmitDefaultValues would restore a cleared repeated field as [].
// Masking must remove the JSON key as well.
func TestMarshalProtoMaskedForRPC_RemovesExcludedKeys(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	decoded := decodeFor(t, srv, newListReply(t), listRPC)

	if _, found := decoded["tags"]; found {
		t.Errorf("tags must be absent, not empty: %v", decoded["tags"])
	}
	if decoded["title"] != "catalog" {
		t.Errorf("title = %v, want catalog", decoded["title"])
	}
	products, ok := decoded["products"].([]any)
	if !ok || len(products) != 2 {
		t.Fatalf("products = %v, want 2 elements", decoded["products"])
	}
	for i, item := range products {
		element, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("products[%d] is not an object: %v", i, item)
		}
		if _, found := element["notes"]; found {
			t.Errorf("products[%d] still carries notes: %v", i, element)
		}
		if _, found := element["name"]; !found {
			t.Errorf("products[%d] lost an unannotated sibling: %v", i, element)
		}
	}
}

func TestMarshalProtoMaskedForRPC_LeavesOtherRPCsIntact(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	reply := newListReply(t)

	for name, rpc := range map[string]string{"detail RPC": getRPC, "no RPC": ""} {
		decoded := decodeFor(t, srv, reply, rpc)
		if _, found := decoded["tags"]; !found {
			t.Errorf("%s: tags was removed although the field does not opt out of it", name)
		}
		element, _ := decoded["products"].([]any)[0].(map[string]any)
		if _, found := element["notes"]; !found {
			t.Errorf("%s: notes was removed although the field does not opt out of it", name)
		}
	}

	plain, err := srv.MarshalProtoMasked(reply)
	if err != nil {
		t.Fatalf("MarshalProtoMasked: %v", err)
	}
	viaEmpty, err := srv.MarshalProtoMaskedForRPC(reply, "")
	if err != nil {
		t.Fatalf("MarshalProtoMaskedForRPC(\"\"): %v", err)
	}
	if string(plain) != string(viaEmpty) {
		t.Errorf("MarshalProtoMasked and MarshalProtoMaskedForRPC(\"\") disagree:\n%s\n%s", plain, viaEmpty)
	}
}

func TestMarshalProtoMaskedForRPC_KeepsFieldLevelExclusions(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	payload, err := srv.MarshalProtoMaskedForRPC(&greeterv1.EchoComplexResponse{
		Name:         "n",
		InternalNote: "secret",
	}, "protomcp.examples.greeter.v1.Greeter.EchoComplex")
	if err != nil {
		t.Fatalf("MarshalProtoMaskedFor: %v", err)
	}
	if strings.Contains(string(payload), "internalNote") {
		t.Errorf("field-level exclude stopped applying once an RPC was named: %s", payload)
	}
	if !strings.Contains(string(payload), `"name":"n"`) {
		t.Errorf("unexcluded field missing: %s", payload)
	}
}

// Without emitted defaults clearing alone removes the key and the JSON
// strip pass is skipped.
func TestMarshalProtoMaskedForRPC_WithoutEmittedDefaults(t *testing.T) {
	srv := protomcp.New("t", "0.0.1",
		protomcp.WithProtoJSONMarshal(protojson.MarshalOptions{}))
	decoded := decodeFor(t, srv, newListReply(t), listRPC)
	if _, found := decoded["tags"]; found {
		t.Errorf("tags must be absent: %v", decoded)
	}
}

func TestMarshalProtoMaskedForRPC_NilMessage(t *testing.T) {
	srv := protomcp.New("t", "0.0.1")
	want, err := srv.MarshalProto(nil)
	if err != nil {
		t.Fatalf("MarshalProto(nil): %v", err)
	}
	got, err := srv.MarshalProtoMaskedForRPC(nil, listRPC)
	if err != nil {
		t.Fatalf("MarshalProtoMaskedForRPC(nil): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("nil handling diverged: got %s, want %s", got, want)
	}
}
