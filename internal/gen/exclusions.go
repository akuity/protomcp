package gen

import (
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

// validateOutputExclusions requires every exclude_from_outputs entry to
// name a tool RPC in this generation request. Imported messages are
// checked too, since they can carry exclusions for the services being
// generated.
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
					"%s: %s.%s: exclude_from_outputs references %q, but no matching tool RPC was found in this "+
						"generation request. Check the name and include the file declaring its service in the run "+
						"(Buf: strategy: all)",
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
