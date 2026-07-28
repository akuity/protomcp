package protomcp_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"

	authv1 "github.com/akuity/protomcp/pkg/api/gen/examples/auth/v1"
	greeterv1 "github.com/akuity/protomcp/pkg/api/gen/examples/greeter/v1"
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
