package protomcp

import (
	"encoding/json"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

// ClearSchemaExcluded zeros every field reachable from m whose descriptor
// carries (protomcp.v1.field_schema).exclude = true. Recursion is capped
// at clearOutputOnlyMaxDepth.
func ClearSchemaExcluded(m proto.Message) {
	clearFieldsMatching(m, isSchemaExcluded)
}

// MarshalProtoMasked serializes m like MarshalProto and then removes every
// (protomcp.v1.field_schema).exclude field from the JSON itself: clearing
// alone is not enough because EmitDefaultValues re-emits cleared fields as
// zero values, leaking the field names the schema masks.
func (s *Server) MarshalProtoMasked(m proto.Message) ([]byte, error) {
	payload, err := s.MarshalProto(m)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return payload, nil
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	stripSchemaExcludedJSON(m.ProtoReflect().Descriptor(), decoded)
	return json.Marshal(decoded)
}

func stripSchemaExcludedJSON(md protoreflect.MessageDescriptor, decoded any) {
	obj, ok := decoded.(map[string]any)
	if !ok {
		return
	}
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if isSchemaExcluded(fd) {
			delete(obj, fd.JSONName())
			delete(obj, string(fd.Name()))
			continue
		}
		value, exists := obj[fd.JSONName()]
		if !exists {
			value, exists = obj[string(fd.Name())]
		}
		if exists {
			stripSchemaExcludedJSONValue(fd, value)
		}
	}
}

func stripSchemaExcludedJSONValue(fd protoreflect.FieldDescriptor, value any) {
	switch {
	case fd.IsMap():
		if fd.MapValue().Kind() != protoreflect.MessageKind {
			return
		}
		entries, ok := value.(map[string]any)
		if !ok {
			return
		}
		for _, entry := range entries {
			stripSchemaExcludedJSON(fd.MapValue().Message(), entry)
		}
	case fd.IsList():
		if fd.Kind() != protoreflect.MessageKind {
			return
		}
		items, ok := value.([]any)
		if !ok {
			return
		}
		for _, item := range items {
			stripSchemaExcludedJSON(fd.Message(), item)
		}
	case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
		stripSchemaExcludedJSON(fd.Message(), value)
	}
}

func isSchemaExcluded(fd protoreflect.FieldDescriptor) bool {
	opts := fd.Options()
	if opts == nil {
		return false
	}
	if !proto.HasExtension(opts, protomcpv1.E_FieldSchema) {
		return false
	}
	fso, _ := proto.GetExtension(opts, protomcpv1.E_FieldSchema).(*protomcpv1.FieldSchemaOptions)
	return fso.GetExclude()
}
