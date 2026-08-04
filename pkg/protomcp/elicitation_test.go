package protomcp

import "testing"

// TestElicitationState pins the helper's contract: deterministic hex
// output, sensitivity to both inputs, and domain separation between
// them. Generated elicitation gates and any test harness invoking a
// gated handler directly both depend on recomputing identical values.
func TestElicitationState(t *testing.T) {
	base := ElicitationState("Tasks_DeleteTask", []byte(`{"id":"a"}`))
	if len(base) != 64 { // hex-encoded sha256
		t.Fatalf("ElicitationState length = %d, want 64 hex chars", len(base))
	}
	if got := ElicitationState("Tasks_DeleteTask", []byte(`{"id":"a"}`)); got != base {
		t.Errorf("not deterministic: %q != %q", got, base)
	}
	if got := ElicitationState("Tasks_DeleteTask", []byte(`{"id":"b"}`)); got == base {
		t.Errorf("argument change did not change the state")
	}
	if got := ElicitationState("Other_Tool", []byte(`{"id":"a"}`)); got == base {
		t.Errorf("tool-name change did not change the state")
	}
	// The 0x00 separator must keep the (toolName, args) boundary
	// unambiguous: shifting bytes across it has to change the state.
	if ElicitationState("ab", []byte("c")) == ElicitationState("a", []byte("bc")) {
		t.Errorf("domain separation broken: boundary shift produced the same state")
	}
}
