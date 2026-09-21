package gen

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

// validateOutputExclusions checks the
// (protomcp.v1.field_schema).exclude_from_outputs entries the current
// run can resolve. They are plain strings the proto compiler never
// looks at, so a typo or a renamed RPC would otherwise leave the field
// in the response without a word.
//
// What a run can resolve is bounded by what protoc hands the plugin:
// the files being generated plus their imports. A file importing one of
// them is absent, which is ordinary rather than exceptional, since
// generation is commonly invoked per directory. An entry is therefore
// an error only when the service it names is in that closure and offers
// no such tool: the service is visible, so the method should be too.
// An entry naming a service from outside the closure is left alone.
//
// Scanning the whole closure rather than only the generated files is
// what keeps the check from depending on which file generation was
// pointed at: a message carrying an entry is usually imported by the
// service that returns it, so generating the service is the run that
// can judge the entry.
func validateOutputExclusions(plugin *protogen.Plugin) error {
	closure := generationClosure(plugin)
	tools, services := annotatedRPCs(plugin, closure)

	for _, f := range plugin.Files {
		if !closure[f.Desc.Path()] {
			continue
		}
		for _, msg := range f.Messages {
			if err := validateMessageOutputExclusions(f, msg, tools, services); err != nil {
				return err
			}
		}
	}
	return nil
}

// generationClosure is every file the run can see: the ones being
// generated and, transitively, the ones they import.
func generationClosure(plugin *protogen.Plugin) map[string]bool {
	closure := map[string]bool{}
	var walk func(fd protoreflect.FileDescriptor)
	walk = func(fd protoreflect.FileDescriptor) {
		path := fd.Path()
		if closure[path] {
			return
		}
		closure[path] = true
		imports := fd.Imports()
		for i := range imports.Len() {
			walk(imports.Get(i).FileDescriptor)
		}
	}
	for _, f := range plugin.Files {
		if f.Generate {
			walk(f.Desc)
		}
	}
	return closure
}

// annotatedRPCs collects the tool-annotated methods in closure, and the
// services declaring any method at all, which is what tells a typo
// apart from a reference this run cannot see.
func annotatedRPCs(plugin *protogen.Plugin, closure map[string]bool) (tools, services map[string]bool) {
	tools, services = map[string]bool{}, map[string]bool{}
	for _, f := range plugin.Files {
		if !closure[f.Desc.Path()] {
			continue
		}
		for _, svc := range f.Services {
			services[string(svc.Desc.FullName())] = true
			for _, m := range svc.Methods {
				if _, ok := toolOptionsFor(m); ok {
					tools[string(m.Desc.FullName())] = true
				}
			}
		}
	}
	return tools, services
}

func validateMessageOutputExclusions(f *protogen.File, msg *protogen.Message, tools, services map[string]bool) error {
	for _, field := range msg.Fields {
		for _, rpc := range excludeFromOutputs(field.Desc) {
			if err := validateOutputExclusion(f, msg, field, rpc, tools, services); err != nil {
				return err
			}
		}
	}
	for _, nested := range msg.Messages {
		if err := validateMessageOutputExclusions(f, nested, tools, services); err != nil {
			return err
		}
	}
	return nil
}

func validateOutputExclusion(f *protogen.File, msg *protogen.Message, field *protogen.Field, rpc string, tools, services map[string]bool) error {
	if tools[rpc] {
		return nil
	}
	service, ok := serviceOf(rpc)
	if !ok {
		return fmt.Errorf(
			"%s: %s.%s: exclude_from_outputs entry %q is not a fully-qualified package.Service.Method name",
			f.Desc.Path(), msg.Desc.FullName(), field.Desc.Name(), rpc)
	}
	if !services[service] {
		return nil
	}
	return fmt.Errorf(
		"%s: %s.%s: exclude_from_outputs names %q, which is not an RPC annotated with protomcp.v1.tool on service %s",
		f.Desc.Path(), msg.Desc.FullName(), field.Desc.Name(), rpc, service)
}

// serviceOf trims the method segment off a fully-qualified RPC name.
func serviceOf(rpc string) (string, bool) {
	dot := strings.LastIndex(rpc, ".")
	if dot <= 0 || dot == len(rpc)-1 {
		return "", false
	}
	return rpc[:dot], true
}

func excludeFromOutputs(fd protoreflect.FieldDescriptor) []string {
	opts := fd.Options()
	if opts == nil || !proto.HasExtension(opts, protomcpv1.E_FieldSchema) {
		return nil
	}
	fso, _ := proto.GetExtension(opts, protomcpv1.E_FieldSchema).(*protomcpv1.FieldSchemaOptions)
	return fso.GetExcludeFromOutputs()
}
