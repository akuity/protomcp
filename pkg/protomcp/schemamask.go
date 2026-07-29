package protomcp

import (
	"encoding/json"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

// ClearSchemaExcluded zeros every field reachable from m whose descriptor
// carries (protomcp.v1.field_schema).exclude = true. Recursion is capped
// at clearOutputOnlyMaxDepth. Any payloads are resolved via
// protoregistry.GlobalTypes; servers configured with a custom protojson
// Resolver should use Server.ClearSchemaExcluded instead.
func ClearSchemaExcluded(m proto.Message) {
	clearFieldsMatching(m, protoregistry.GlobalTypes, isSchemaExcluded)
}

// ClearSchemaExcluded is like the package-level ClearSchemaExcluded but
// resolves google.protobuf.Any payloads through the Resolver configured
// via WithProtoJSONUnmarshal, falling back to protoregistry.GlobalTypes.
func (s *Server) ClearSchemaExcluded(m proto.Message) {
	clearFieldsMatching(m, s.unmarshalResolver(), isSchemaExcluded)
}

// MarshalProtoMasked clears every (protomcp.v1.field_schema).exclude field
// on a clone of m (so excluded values never reach protojson and m stays
// intact for trusted result processors) and serializes the clone like
// MarshalProto. When the configured MarshalOptions emit unset fields
// (EmitDefaultValues / EmitUnpopulated), the excluded field names are
// additionally removed from the JSON itself: clearing alone is not enough
// because those options re-emit cleared fields as zero values, leaking the
// field names the schema masks. Content the masking walk cannot verify —
// an unresolvable or corrupt Any payload, or nesting beyond the depth
// bound — is an error, never a silently truncated success.
func (s *Server) MarshalProtoMasked(m proto.Message) ([]byte, error) {
	if m == nil {
		return s.MarshalProto(m)
	}
	masked := proto.Clone(m)
	if err := clearFieldsMatchingMode(masked, s.marshalResolver(), clearFailLoud, isSchemaExcluded); err != nil {
		return nil, err
	}
	payload, err := s.MarshalProto(masked)
	if err != nil {
		return nil, err
	}
	if !s.protoMarshal.EmitDefaultValues && !s.protoMarshal.EmitUnpopulated {
		return payload, nil
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	stripSchemaExcludedJSON(masked.ProtoReflect().Descriptor(), decoded, s.marshalResolver())
	if s.protoMarshal.Multiline || s.protoMarshal.Indent != "" {
		indent := s.protoMarshal.Indent
		if indent == "" {
			indent = "  "
		}
		return json.MarshalIndent(decoded, "", indent)
	}
	return json.Marshal(decoded)
}

func stripSchemaExcludedJSON(md protoreflect.MessageDescriptor, decoded any, resolver anyTypeResolver) {
	obj, ok := decoded.(map[string]any)
	if !ok {
		return
	}
	if md.FullName() == anyMessageName {
		stripSchemaExcludedAnyJSON(md, obj, resolver)
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
			stripSchemaExcludedJSONValue(fd, value, resolver)
		}
	}
}

func stripSchemaExcludedAnyJSON(anyMD protoreflect.MessageDescriptor, obj map[string]any, resolver anyTypeResolver) {
	typeURL, _ := obj["@type"].(string)
	mt, err := resolver.FindMessageByURL(typeURL)
	if err != nil {
		for k := range obj {
			if k != "@type" {
				delete(obj, k)
			}
		}
		return
	}
	md := mt.Descriptor()
	switch {
	case md.FullName() == anyMessageName:
		stripSchemaExcludedJSON(anyMD, obj["value"], resolver)
	case protojsonCustomWKTs[md.FullName()]:
		return
	default:
		stripSchemaExcludedJSON(md, obj, resolver)
	}
}

// protojsonCustomWKTs is the exact set of types protojson renders with a
// custom JSON shape. Only these may be skipped by the strip pass; every
// other message — including non-WKT types that merely live in the
// google.protobuf package — is stripped field-by-field.
var protojsonCustomWKTs = map[protoreflect.FullName]bool{
	"google.protobuf.Any":         true,
	"google.protobuf.Timestamp":   true,
	"google.protobuf.Duration":    true,
	"google.protobuf.Struct":      true,
	"google.protobuf.Value":       true,
	"google.protobuf.ListValue":   true,
	"google.protobuf.FieldMask":   true,
	"google.protobuf.Empty":       true,
	"google.protobuf.BoolValue":   true,
	"google.protobuf.BytesValue":  true,
	"google.protobuf.DoubleValue": true,
	"google.protobuf.FloatValue":  true,
	"google.protobuf.Int32Value":  true,
	"google.protobuf.Int64Value":  true,
	"google.protobuf.StringValue": true,
	"google.protobuf.UInt32Value": true,
	"google.protobuf.UInt64Value": true,
}

func stripSchemaExcludedJSONValue(fd protoreflect.FieldDescriptor, value any, resolver anyTypeResolver) {
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
			stripSchemaExcludedJSON(fd.MapValue().Message(), entry, resolver)
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
			stripSchemaExcludedJSON(fd.Message(), item, resolver)
		}
	case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
		stripSchemaExcludedJSON(fd.Message(), value, resolver)
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
