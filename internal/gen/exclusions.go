package gen

import (
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

// validateOutputExclusions rejects every
// (protomcp.v1.field_schema).exclude_from_outputs entry in a generated
// file that does not name a tool-annotated RPC somewhere in the
// generation set. The entries are plain strings the proto compiler
// never resolves, so a typo or a renamed RPC would otherwise leave the
// field in place without a word.
func validateOutputExclusions(plugin *protogen.Plugin) error {
	tools := map[string]bool{}
	for _, f := range plugin.Files {
		for _, svc := range f.Services {
			for _, m := range svc.Methods {
				if _, ok := toolOptionsFor(m); ok {
					tools[string(m.Desc.FullName())] = true
				}
			}
		}
	}
	for _, f := range plugin.Files {
		if !f.Generate {
			continue
		}
		for _, msg := range f.Messages {
			if err := validateMessageOutputExclusions(f, msg, tools); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMessageOutputExclusions(f *protogen.File, msg *protogen.Message, tools map[string]bool) error {
	for _, field := range msg.Fields {
		for _, rpc := range excludeFromOutputs(field.Desc) {
			if !tools[rpc] {
				return fmt.Errorf(
					"%s: %s.%s: exclude_from_outputs names %q, which is not an RPC annotated with protomcp.v1.tool in this generation set",
					f.Desc.Path(), msg.Desc.FullName(), field.Desc.Name(), rpc)
			}
		}
	}
	for _, nested := range msg.Messages {
		if err := validateMessageOutputExclusions(f, nested, tools); err != nil {
			return err
		}
	}
	return nil
}

func excludeFromOutputs(fd protoreflect.FieldDescriptor) []string {
	opts := fd.Options()
	if opts == nil || !proto.HasExtension(opts, protomcpv1.E_FieldSchema) {
		return nil
	}
	fso, _ := proto.GetExtension(opts, protomcpv1.E_FieldSchema).(*protomcpv1.FieldSchemaOptions)
	return fso.GetExcludeFromOutputs()
}
