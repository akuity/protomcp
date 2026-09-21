package schema

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

const (
	listRPC = "schematest.v1.Inventory.ListProducts"
	getRPC  = "schematest.v1.Inventory.GetProduct"
)

// ratingsDescriptor builds a message whose repeated `reviews` field opts
// out of listRPC's output, next to an unannotated `review_count`.
func ratingsDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	reviewsOpts := &descriptorpb.FieldOptions{}
	proto.SetExtension(reviewsOpts, protomcpv1.E_FieldSchema,
		&protomcpv1.FieldSchemaOptions{ExcludeFromOutputs: []string{listRPC}})
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("schematest/ratings.proto"),
		Package: proto.String("schematest.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Review"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:     proto.String("body"),
					Number:   proto.Int32(1),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					JsonName: proto.String("body"),
				}},
			},
			{
				Name: proto.String("Ratings"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("review_count"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("reviewCount"),
					},
					{
						Name:     proto.String("reviews"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".schematest.v1.Review"),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						JsonName: proto.String("reviews"),
						Options:  reviewsOpts,
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd.Messages().ByName("Ratings")
}

func TestForOutputOmitsFieldsExcludedFromThisRPC(t *testing.T) {
	md := ratingsDescriptor(t)

	forList := props(t, jsonRound(t, ForOutput(md, Options{RPCFullName: listRPC})))
	if _, found := forList["reviews"]; found {
		t.Error("reviews is still in the output schema of the RPC it opts out of")
	}
	if _, found := forList["reviewCount"]; !found {
		t.Error("an unannotated sibling was dropped alongside the excluded field")
	}

	for name, opts := range map[string]Options{
		"another RPC": {RPCFullName: getRPC},
		"no RPC":      {},
	} {
		if _, found := props(t, jsonRound(t, ForOutput(md, opts)))["reviews"]; !found {
			t.Errorf("%s: reviews must stay in the output schema", name)
		}
	}
}

// TestForInputIgnoresOutputExclusions pins the direction of the option:
// it shapes what an RPC returns, never what a client may send.
func TestForInputIgnoresOutputExclusions(t *testing.T) {
	md := ratingsDescriptor(t)
	if _, found := props(t, jsonRound(t, ForInput(md, Options{RPCFullName: listRPC})))["reviews"]; !found {
		t.Error("exclude_from_outputs removed a field from an input schema")
	}
}

func TestHasOutputExclusions(t *testing.T) {
	md := ratingsDescriptor(t)
	if !HasOutputExclusions(md, listRPC) {
		t.Error("HasOutputExclusions(listRPC) = false, want true")
	}
	if HasOutputExclusions(md, getRPC) {
		t.Error("HasOutputExclusions(getRPC) = true, want false")
	}
	if HasExclusions(md) {
		t.Error("HasExclusions = true; exclude_from_outputs alone must not count as a field-level exclusion")
	}
}

func props(t *testing.T, s map[string]any) map[string]any {
	t.Helper()
	p, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties: %v", s)
	}
	return p
}
