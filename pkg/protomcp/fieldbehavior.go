package protomcp

import (
	"fmt"
	"slices"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const anyMessageName protoreflect.FullName = "google.protobuf.Any"

// anyTypeResolver resolves a google.protobuf.Any type_url to a message
// type; satisfied by protoregistry.GlobalTypes and the Resolver field of
// protojson.MarshalOptions / protojson.UnmarshalOptions.
type anyTypeResolver interface {
	FindMessageByURL(url string) (protoreflect.MessageType, error)
}

// clearOutputOnlyMaxDepth bounds recursion so self-referential protos
// (Struct/Value) cannot exhaust the stack on a malicious payload. It
// matches protojson's default unmarshal recursion limit, so no message
// parsed with default options can nest deeper than the masking walk
// reaches. At the limit the fail-closed (input) helpers clear the whole
// remaining subtree; the fail-loud (output) paths return an error.
const clearOutputOnlyMaxDepth = 10000

// clearMode selects how the masking walk handles content it cannot
// verify: input helpers fail closed (wipe it and continue), output
// masking fails loud (return an error; never serialize unverified data
// as success).
type clearMode int

const (
	clearFailClosed clearMode = iota
	clearFailLoud
)

// ClearOutputOnly zeros every field reachable from m whose descriptor
// carries google.api.field_behavior = OUTPUT_ONLY (AIP-203). Recursion
// is capped at clearOutputOnlyMaxDepth. Any payloads are resolved via
// protoregistry.GlobalTypes; servers configured with a custom protojson
// Resolver should use Server.ClearOutputOnly instead.
func ClearOutputOnly(m proto.Message) {
	clearFieldsMatching(m, protoregistry.GlobalTypes, hasOutputOnly)
}

// ClearOutputOnly is like the package-level ClearOutputOnly but resolves
// google.protobuf.Any payloads through the Resolver configured via
// WithProtoJSONUnmarshal, falling back to protoregistry.GlobalTypes.
func (s *Server) ClearOutputOnly(m proto.Message) {
	clearFieldsMatching(m, s.unmarshalResolver(), hasOutputOnly)
}

func clearFieldsMatching(m proto.Message, resolver anyTypeResolver, match func(protoreflect.FieldDescriptor) bool) {
	_ = clearFieldsMatchingMode(m, resolver, clearFailClosed, match)
}

func clearFieldsMatchingMode(m proto.Message, resolver anyTypeResolver, mode clearMode, match func(protoreflect.FieldDescriptor) bool) error {
	if m == nil {
		return nil
	}
	r := m.ProtoReflect()
	if !r.IsValid() {
		return nil
	}
	return clearFieldsMatchingReflect(r, resolver, 0, mode, match)
}

// clearFieldsMatchingReflect is the recursive worker; depth is bounded
// by clearOutputOnlyMaxDepth per clearMode.
func clearFieldsMatchingReflect(r protoreflect.Message, resolver anyTypeResolver, depth int, mode clearMode, match func(protoreflect.FieldDescriptor) bool) error {
	if !r.IsValid() {
		return nil
	}
	if depth >= clearOutputOnlyMaxDepth {
		if mode == clearFailLoud {
			return fmt.Errorf(
				"protomcp: masking aborted: %s nests deeper than %d levels; refusing to serialize content the masking walk cannot verify",
				r.Descriptor().FullName(), clearOutputOnlyMaxDepth)
		}
		clearAllFields(r)
		return nil
	}
	if r.Descriptor().FullName() == anyMessageName {
		return clearMatchingInAnyPayload(r, resolver, depth, mode, match)
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
		// is checked in clearFieldChildren.
		if fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind {
			continue
		}
		if !r.Has(fd) {
			continue
		}
		if err := clearFieldChildren(r, fd, resolver, depth, mode, match); err != nil {
			return err
		}
	}
	return nil
}

func clearFieldChildren(r protoreflect.Message, fd protoreflect.FieldDescriptor, resolver anyTypeResolver, depth int, mode clearMode, match func(protoreflect.FieldDescriptor) bool) error {
	switch {
	case fd.IsMap():
		if fd.MapValue().Kind() != protoreflect.MessageKind {
			return nil
		}
		var rangeErr error
		r.Get(fd).Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
			rangeErr = clearFieldsMatchingReflect(v.Message(), resolver, depth+1, mode, match)
			return rangeErr == nil
		})
		return rangeErr
	case fd.IsList():
		list := r.Get(fd).List()
		for j := range list.Len() {
			if err := clearFieldsMatchingReflect(list.Get(j).Message(), resolver, depth+1, mode, match); err != nil {
				return err
			}
		}
		return nil
	default:
		return clearFieldsMatchingReflect(r.Get(fd).Message(), resolver, depth+1, mode, match)
	}
}

func clearAllFields(r protoreflect.Message) {
	fields := r.Descriptor().Fields()
	for i := range fields.Len() {
		r.Clear(fields.Get(i))
	}
}

func clearMatchingInAnyPayload(r protoreflect.Message, resolver anyTypeResolver, depth int, mode clearMode, match func(protoreflect.FieldDescriptor) bool) error {
	fields := r.Descriptor().Fields()
	urlFD := fields.ByName("type_url")
	valueFD := fields.ByName("value")
	raw := r.Get(valueFD).Bytes()
	if len(raw) == 0 {
		return nil
	}
	failClosed := func() {
		r.Clear(urlFD)
		r.Clear(valueFD)
	}
	url := r.Get(urlFD).String()
	mt, err := resolver.FindMessageByURL(url)
	if err != nil {
		if mode == clearFailLoud {
			return fmt.Errorf("protomcp: masking aborted: cannot resolve Any type %q: %w", url, err)
		}
		failClosed()
		return nil
	}
	if !descriptorHasMatch(mt.Descriptor(), match, map[protoreflect.FullName]bool{}) {
		return nil
	}
	payload := mt.New()
	if uErr := proto.Unmarshal(raw, payload.Interface()); uErr != nil {
		if mode == clearFailLoud {
			return fmt.Errorf("protomcp: masking aborted: cannot unmarshal Any payload of type %q: %w", url, uErr)
		}
		failClosed()
		return nil
	}
	if cErr := clearFieldsMatchingReflect(payload, resolver, depth+1, mode, match); cErr != nil {
		return cErr
	}
	repacked, mErr := proto.Marshal(payload.Interface())
	if mErr != nil {
		if mode == clearFailLoud {
			return fmt.Errorf("protomcp: masking aborted: cannot repack Any payload of type %q: %w", url, mErr)
		}
		failClosed()
		return nil
	}
	r.Set(valueFD, protoreflect.ValueOfBytes(repacked))
	return nil
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
