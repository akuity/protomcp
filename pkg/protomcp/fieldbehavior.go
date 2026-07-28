package protomcp

import (
	"slices"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const anyMessageName protoreflect.FullName = "google.protobuf.Any"

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
	if depth >= clearOutputOnlyMaxDepth || !r.IsValid() {
		return
	}
	if r.Descriptor().FullName() == anyMessageName {
		clearMatchingInAnyPayload(r, depth, match)
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

func clearMatchingInAnyPayload(r protoreflect.Message, depth int, match func(protoreflect.FieldDescriptor) bool) {
	fields := r.Descriptor().Fields()
	urlFD := fields.ByName("type_url")
	valueFD := fields.ByName("value")
	raw := r.Get(valueFD).Bytes()
	if len(raw) == 0 {
		return
	}
	clearAll := func() {
		r.Clear(urlFD)
		r.Clear(valueFD)
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByURL(r.Get(urlFD).String())
	if err != nil {
		clearAll()
		return
	}
	if !descriptorHasMatch(mt.Descriptor(), match, map[protoreflect.FullName]bool{}) {
		return
	}
	payload := mt.New()
	if uErr := proto.Unmarshal(raw, payload.Interface()); uErr != nil {
		clearAll()
		return
	}
	clearFieldsMatchingReflect(payload, depth+1, match)
	repacked, mErr := proto.Marshal(payload.Interface())
	if mErr != nil {
		clearAll()
		return
	}
	r.Set(valueFD, protoreflect.ValueOfBytes(repacked))
}

func descriptorHasMatch(md protoreflect.MessageDescriptor, match func(protoreflect.FieldDescriptor) bool, seen map[protoreflect.FullName]bool) bool {
	if md.FullName() == anyMessageName {
		return true
	}
	if seen[md.FullName()] {
		return false
	}
	seen[md.FullName()] = true
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if match(fd) {
			return true
		}
		switch {
		case fd.IsMap():
			if fd.MapValue().Kind() == protoreflect.MessageKind && descriptorHasMatch(fd.MapValue().Message(), match, seen) {
				return true
			}
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			if descriptorHasMatch(fd.Message(), match, seen) {
				return true
			}
		}
	}
	return false
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
