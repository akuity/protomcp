package protomcp_test

import (
	"encoding/json"
	"testing"

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
