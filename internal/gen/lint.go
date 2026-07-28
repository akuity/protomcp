package gen

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"

	protomcpv1 "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

var mutatingVerbs = []string{
	"Add", "Apply", "Create", "Delete", "Destroy", "Disable", "Enable",
	"Grant", "Insert", "Patch", "Purge", "Put", "Remove", "Reset",
	"Restart", "Revoke", "Rotate", "Set", "Terminate", "Update", "Upsert",
	"Write",
}

func mutatingVerbPrefix(name string) (string, bool) {
	for _, v := range mutatingVerbs {
		rest, ok := strings.CutPrefix(name, v)
		if !ok {
			continue
		}
		if rest == "" {
			return v, true
		}
		if c := rest[0]; (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			return v, true
		}
	}
	return "", false
}

func validateReadOnlyHint(svc *protogen.Service, m *protogen.Method, to *protomcpv1.ToolOptions) error {
	if !to.GetReadOnly() {
		return nil
	}
	verb, ok := mutatingVerbPrefix(m.GoName)
	if !ok {
		return nil
	}
	return fmt.Errorf(
		"%s.%s: protomcp.v1.tool sets read_only: true on an RPC whose name "+
			"starts with the mutating verb %q; a read-only hint on a mutating "+
			"RPC misleads MCP clients into calling it without user consent "+
			"(rename the RPC or drop read_only)",
		svc.GoName, m.GoName, verb,
	)
}
