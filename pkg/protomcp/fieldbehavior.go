package protomcp

import (
	"slices"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// clearOutputOnlyMaxDepth bounds recursion so self-referential protos
// (Struct/Value) cannot exhaust the stack on a malicious payload.
// Fields below the limit retain their caller-supplied value.
const clearOutputOnlyMaxDepth = 100

// ClearOutputOnly zeros every field reachable from m whose descriptor
// carries google.api.field_behavior = OUTPUT_ONLY (AIP-203). Recursion
// is capped at clearOutputOnlyMaxDepth.
func ClearOutputOnly(m proto.Message) {
	clearFieldsMatching(m, hasOutputOnly)
}

func clearFieldsMatching(m proto.Message, match func(protoreflect.FieldDescriptor) bool) {
	if m == nil {
		return
	}
	r := m.ProtoReflect()
	if !r.IsValid() {
		return
	}
	clearFieldsMatchingReflect(r, 0, match)
}

// clearFieldsMatchingReflect is the recursive worker; depth
// short-circuits at clearOutputOnlyMaxDepth.
func clearFieldsMatchingReflect(r protoreflect.Message, depth int, match func(protoreflect.FieldDescriptor) bool) {
	if depth >= clearOutputOnlyMaxDepth {
		return
	}
	fields := r.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)

		if match(fd) {
			r.Clear(fd)
			continue
		}

		// Only message-valued branches need recursion. For maps the
		// field Kind is MessageKind (the map entry); MapValue().Kind()
		// is checked below.
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			continue
		}
		if !r.Has(fd) {
			continue
		}

		switch {
		case fd.IsMap():
			if fd.MapValue().Kind() != protoreflect.MessageKind {
				continue
			}
			r.Get(fd).Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
				clearFieldsMatchingReflect(v.Message(), depth+1, match)
				return true
			})
		case fd.IsList():
			list := r.Get(fd).List()
			for j := range list.Len() {
				clearFieldsMatchingReflect(list.Get(j).Message(), depth+1, match)
			}
		default:
			clearFieldsMatchingReflect(r.Get(fd).Message(), depth+1, match)
		}
	}
}

// hasOutputOnly reports whether fd's FieldBehavior list includes
// OUTPUT_ONLY. AIP-203 allows multiple behaviors per field.
func hasOutputOnly(fd protoreflect.FieldDescriptor) bool {
	opts := fd.Options()
	if opts == nil {
		return false
	}
	if !proto.HasExtension(opts, annotations.E_FieldBehavior) {
		return false
	}
	behaviors, _ := proto.GetExtension(opts, annotations.E_FieldBehavior).([]annotations.FieldBehavior)
	return slices.Contains(behaviors, annotations.FieldBehavior_OUTPUT_ONLY)
}
