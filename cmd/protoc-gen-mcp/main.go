// Command protoc-gen-mcp is a protoc plugin that generates Go code which
// registers annotated gRPC methods as MCP tools against a protomcp.Server.
//
// The plugin is driven by the standard google.golang.org/protobuf/compiler/
// protogen harness; it is invoked as part of `buf generate` or
// `protoc --mcp_out=...` and emits one <file>.mcp.pb.go per input .proto
// file that contains at least one annotated RPC.
package main

import (
	"flag"

	"google.golang.org/protobuf/compiler/protogen"

	"github.com/akuity/protomcp/internal/gen"
)

func main() {
	var flags flag.FlagSet
	maxDepth := flags.Int("max_recursion_depth", 0,
		"maximum recursive-message expansion depth in generated JSON schemas "+
			"(0 uses the library default of 3)")
	maxSchemaBytes := flags.Int("max_tool_schema_bytes", 0,
		"maximum combined serialized size in bytes of a single tool's input and "+
			"output JSON schemas; exceeding it fails generation (0 disables)")
	readOnlyNameLint := flags.Bool("read_only_name_lint", true,
		"reject read_only: true on RPCs whose name starts with a mutating verb; "+
			"set to false when a legitimately read-only RPC trips the heuristic")

	protogen.Options{ParamFunc: flags.Set}.Run(func(p *protogen.Plugin) error {
		return gen.GenerateWithOptions(p, gen.Options{
			MaxRecursionDepth:       *maxDepth,
			MaxToolSchemaBytes:      *maxSchemaBytes,
			DisableReadOnlyNameLint: !*readOnlyNameLint,
		})
	})
}
