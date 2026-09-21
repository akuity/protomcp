package gen

import (
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

// validateOutputExclusions rejects every
// (protomcp.v1.field_schema).exclude_from_outputs entry among the files
// handed to this run that does not name a tool-annotated RPC among those
// same files. The entries are plain strings the proto compiler never
// resolves, so a typo or a renamed RPC would otherwise leave the field
// in the response without a word.
//
// A run sees the files being generated and, transitively, the ones they
// import. An entry therefore resolves when the RPC is declared in the
// same file as the field, or in a file generated alongside it. A layout
// that keeps the message in a file the service imports needs the module
// generated in one run (buf: strategy: all) for the entry to resolve,
// and the error says so.
//
// Every file in the request is scanned, not only the generated ones, so
// an entry on an imported message is checked by the run that generates
// the service returning it.
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
					"%s: %s.%s: exclude_from_outputs names %q, which is not an RPC annotated with protomcp.v1.tool "+
						"among the files in this generation run; when the RPC is declared in a file that imports "+
						"this one, generate the module in a single run (buf: strategy: all)",
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
