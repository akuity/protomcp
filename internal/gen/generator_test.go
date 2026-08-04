package gen

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	// Side-effect imports: register the annotation extension and the fixture
	// protos in the global protoregistry so protogen can resolve extension
	// types when walking the descriptors embedded in fixtures.binpb.
	_ "github.com/akuity/protomcp/internal/gen/testdata/elicit"
	_ "github.com/akuity/protomcp/internal/gen/testdata/greeter"
	_ "github.com/akuity/protomcp/internal/gen/testdata/multi"
	_ "github.com/akuity/protomcp/internal/gen/testdata/notools"
	_ "github.com/akuity/protomcp/internal/gen/testdata/options"
	_ "github.com/akuity/protomcp/internal/gen/testdata/pragmas"
	_ "github.com/akuity/protomcp/internal/gen/testdata/prompts"
	_ "github.com/akuity/protomcp/pkg/api/gen/protomcp/v1"
)

// fixturesBin is the output of, from internal/gen/testdata (DEPS holds
// buf/validate/validate.proto and google/api/field_behavior.proto, e.g.
// via `buf export buf.build/bufbuild/protovalidate -o "$DEPS"`):
//
//	protoc -I . -I ../../../proto -I "$DEPS" \
//	    --include_source_info --include_imports \
//	    --descriptor_set_out=fixtures.binpb \
//	    $(ls *.proto | LC_ALL=C sort)
//
// The C-collation sort keeps the file order (and therefore the bytes)
// reproducible when a fixture is added.
//
// It holds FileDescriptorProto messages for every testdata fixture, with
// source_code_info populated so our leading-comment-fallback assertions
// actually have comment text to observe (the runtime protoregistry does
// not carry source info).
//
//go:embed testdata/fixtures.binpb
var fixturesBin []byte

// TestGenerate_Greeter drives the real generator against the committed
// greeter.proto fixture and asserts the resulting *.mcp.pb.go contains
// every expected symbol (and none of the ones that should be skipped).
func TestGenerate_Greeter(t *testing.T) {
	out := runGenerate(t, "greeter.proto")

	cases := []substringCase{
		{"register function", true, "RegisterGreeterMCPTools"},
		{"SayHello tool", true, `"Greeter_SayHello"`},
		{"StreamGreetings tool", true, `"Greeter_StreamGreetings"`},
		{"unannotated Internal RPC is not exposed", false, "\"Greeter_Internal\""},
		{"unannotated Internal RPC does not appear at all", false, "Greeter_Internal_InputSchema"},
		{"unannotated BatchGreet (client-streaming) is not exposed", false, `"Greeter_BatchGreet"`},
		{"unannotated Chat (bidi) is not exposed", false, `"Greeter_Chat"`},
		// Skip comments were removed, unsupported streaming shapes now
		// either produce nothing (when unannotated) or a hard error (when
		// annotated; see TestGenerate_BadStreams_ClientErrors / _BidiErrors).
		{"no skip comments in output", false, "protoc-gen-mcp: skipping"},
		{"server-streaming emits progress loop", true, "NotifyProgress"},
		{"unary handler path", true, "client.SayHello(ctx, upstream)"},
		{"streaming handler path", true, "client.StreamGreetings(ctx, upstream)"},
		{"reads Input from GRPCData (type-assert)", true, "g.Input.(*"},
		// Client-controlled progress-token values MUST be sanitized
		// before landing in outgoing gRPC metadata (CR/LF/NUL stripped).
		{"progress token sanitized before Metadata.Set", true, "protomcp.SanitizeMetadataValue(fmt.Sprintf"},
	}
	assertSubstrings(t, out, cases)
}

// TestGenerate_BadStreams_ClientErrors asserts the generator returns a
// clear error when a client-streaming RPC is annotated with protomcp.v1.tool.
func TestGenerate_BadStreams_ClientErrors(t *testing.T) {
	err := runGenerateExpectError(t, "bad_streams.proto")
	want := "BadClient.Push: client-streaming RPCs cannot be exposed as MCP primitives"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

// TestGenerate_BadStreams_BidiErrors asserts the generator returns a
// clear error when a bidi-streaming RPC is annotated with protomcp.v1.tool.
func TestGenerate_BadStreams_BidiErrors(t *testing.T) {
	err := runGenerateExpectError(t, "bad_bidi.proto")
	want := "BadBidi.Duplex: bidi-streaming RPCs cannot be exposed as MCP primitives"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

// TestGenerate_Elicit covers the happy path where a method carries both a
// tool and an elicitation annotation: the generated source must emit the
// multi-round-trip confirmation gate — an InputRequests result carrying
// mcp.ElicitParams on the first invocation, the inputResponses lookup on
// the retry — plus the Mustache-rendered message expression and the
// decline-path IsError short-circuit.
func TestGenerate_Elicit(t *testing.T) {
	out := runGenerate(t, "elicit.proto")

	cases := []substringCase{
		{"register function", true, "RegisterElicitMCPTools"},
		{"Delete tool name", true, `"Elicit_Delete"`},
		{"ElicitParams struct literal", true, "&mcp.ElicitParams{"},
		// First invocation publishes the confirmation under the fixed
		// server-assigned request ID.
		{"InputRequests map literal", true, "InputRequests: mcp.InputRequestMap{"},
		// The retry reads the client's echoed answer back by the same key.
		{"inputResponses lookup", true, `req.Params.InputResponses["confirm"]`},
		{"answer type assertion", true, "*mcp.ElicitResult"},
		// The old direct server-initiated request must be gone: it hard-fails
		// on protocol >= 2026-07-28 sessions.
		{"no direct Elicit call", false, ".Elicit(ctx"},
		// The literal prefix up to the first Mustache var appears as a Go
		// string literal in the emitted Sprintf concatenation.
		{"rendered message prefix", true, `"Delete item with id "`},
		{"rendered message id getter", true, "(&in).GetId()"},
		// Non-accept actions short-circuit with an IsError result.
		{"decline short-circuit", true, "User declined to proceed."},
		{"IsError on decline", true, "IsError: true"},
		// The gRPC call still appears, elicitation wraps it, not replaces.
		{"delete still calls gRPC", true, "client.Delete(ctx, upstream)"},
		// Destructive tools still get the DestructiveHint annotation.
		{"destructive hint preserved", true, "DestructiveHint: protomcp.BoolPtr(true)"},
	}
	assertSubstrings(t, out, cases)
}

// TestGenerate_BadDupURI asserts the generator hard-errors when two
// `resource_list` annotations appear in the same codegen run. MCP's
// `resources/list` is a single flat cursor-paginated stream; running
// two listers against it would produce non-deterministic pagination.
// Users enumerate multiple resource types via a single RPC + a
// templated URI scheme like `{type}://{id}`.
func TestGenerate_BadDupURI(t *testing.T) {
	err := runGenerateExpectError(t, "bad_dup_uri.proto")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{
		"at most one resource_list",
		"already registered",
		"{type}://{id}", // the suggested fix appears in the error
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nerror: %v", want, err)
		}
	}
}

// TestGenerate_BadDupListChanged asserts the generator hard-errors
// when two `resource_list_changed` annotations appear in one codegen
// run. Every annotation fires the same single
// `notifications/resources/list_changed` wire event, so multiple
// annotations are always redundant.
func TestGenerate_BadDupListChanged(t *testing.T) {
	err := runGenerateExpectError(t, "bad_dup_list_changed.proto")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{
		"at most one resource_list_changed",
		"already registered",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nerror: %v", want, err)
		}
	}
}

// TestGenerate_ResourceTemplateOnly covers a service whose only
// annotation is a `resource_template`: no tool, no resource_list, no
// prompt. Only resource_list.go.tmpl renders Mustache and decodes items
// with encoding/json, so a resource-read-only file must not import
// either package — protogen never prunes imports, so an import the
// templates do not reference is an "imported and not used" compile
// error in the user's build.
func TestGenerate_ResourceTemplateOnly(t *testing.T) {
	out := runGenerate(t, "resource_template_only.proto")

	cases := []substringCase{
		{"resources register function", true, "func RegisterCatalogMCPResources("},
		{"claiming AddResourceTemplate", true, "srv.MustAddResourceTemplate("},
		{"uri template var declared", true, `MustNew("catalog://{id}")`},
		{"uritemplate is still imported", true, `"github.com/yosida95/uritemplate/v3"`},
		{"no tool registration", false, "MustAddTool"},
		{"no resource lister", false, "RegisterResourceLister"},
		{"mustache is not imported", false, `"github.com/cbroglie/mustache"`},
		{"mustache is never rendered", false, "mustache.Render"},
		{"encoding/json is not imported", false, `"encoding/json"`},
	}
	assertSubstrings(t, out, cases)
}

// TestGenerate_BadElicitNoTool asserts the generator hard-errors on a
// method annotated with protomcp.v1.elicitation but no protomcp.v1.tool ,
// elicitation is a modifier and has nothing to gate on its own.
func TestGenerate_BadElicitNoTool(t *testing.T) {
	err := runGenerateExpectError(t, "bad_elicit_no_tool.proto")
	want := "BadElicitNoTool.Act: protomcp.v1.elicitation requires a protomcp.v1.tool"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

// TestGenerate_BadElicitSection asserts the generator hard-errors on an
// elicitation message that uses Mustache section syntax. Our contract is
// logic-less rendering, sections would require runtime condition
// evaluation over the proto request and we do not support that.
func TestGenerate_BadElicitSection(t *testing.T) {
	err := runGenerateExpectError(t, "bad_elicit_section.proto")
	// Error wording comes from schema.ParseMustache; assert on the stable part.
	want := "sections"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestGenerate_Slim(t *testing.T) {
	out := runGenerate(t, "slim.proto")

	cases := []substringCase{
		{"input clears excluded fields", true, "srv.ClearSchemaExcluded(&in)"},
		{"output is never cleared; g.Output stays raw for result processors", false, "ClearSchemaExcluded(resp)"},
		{"streaming output is never cleared", false, "ClearSchemaExcluded(msg)"},
		{"kept input field stays in schema", true, `"id"`},
		{"kept nested field stays in schema", true, `"keep"`},
		{"kept output field stays in schema", true, `"summary"`},
		{"excluded input field masked from schema", false, "hugeSelector"},
		{"excluded nested field masked from schema", false, `"omit"`},
		{"excluded output field masked from schema", false, "debugBlob"},
	}
	assertSubstrings(t, out, cases)

	if got := strings.Count(out, "ClearSchemaExcluded(&in)"); got != 2 {
		t.Errorf("ClearSchemaExcluded(&in) emitted %d times, want 2 (the two tools)", got)
	}
	if got := strings.Count(out, "MarshalProtoMasked(resp)"); got != 3 {
		t.Errorf("MarshalProtoMasked(resp) emitted %d times, want 3 "+
			"(unary tool, resource read, prompt)\n--- file ---\n%s", got, out)
	}
	if got := strings.Count(out, "MarshalProtoMasked(msg)"); got != 1 {
		t.Errorf("MarshalProtoMasked(msg) emitted %d times, want 1 (streaming tool)", got)
	}
	if got := strings.Count(out, "MarshalProtoMasked(item)"); got != 1 {
		t.Errorf("MarshalProtoMasked(item) emitted %d times, want 1 (resource list)", got)
	}

	moreCases := []substringCase{
		{"REQUIRED+OUTPUT_ONLY+exclude generates fine and stays masked", false, "serverRef"},
		{"excluded prompt request field is not a prompt argument", false, `"trace"`},
		// Control for TestGenerate_ResourceTemplateOnly: this fixture does
		// carry a resource_list, so the mustache import must stay.
		{"resource_list keeps the mustache import", true, `"github.com/cbroglie/mustache"`},
		{"resource_list renders name_field with mustache", true, "mustache.Render"},
	}
	assertSubstrings(t, out, moreCases)
}

func TestGenerate_BadSlimBinding(t *testing.T) {
	err := runGenerateExpectError(t, "bad_slim_binding.proto")
	want := `masked by (protomcp.v1.field_schema).exclude`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestGenerate_BadSlimProjection(t *testing.T) {
	err := runGenerateExpectError(t, "bad_slim_projection.proto")
	want := `masked by (protomcp.v1.field_schema).exclude`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestGenerate_BadSlimPromptVar(t *testing.T) {
	err := runGenerateExpectError(t, "bad_slim_prompt_var.proto")
	want := `masked by (protomcp.v1.field_schema).exclude`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestGenerate_ReadOnlyNameLintDisabled(t *testing.T) {
	req := buildGenRequest(t, "bad_read_only_mutating.proto")
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	if err := GenerateWithOptions(plugin, Options{DisableReadOnlyNameLint: true}); err != nil {
		t.Fatalf("GenerateWithOptions: %v", err)
	}
	if resp := plugin.Response(); resp.Error != nil {
		t.Fatalf("plugin error: %s", *resp.Error)
	}
}

func TestGenerate_AnyMasking(t *testing.T) {
	out := runGenerate(t, "any_masking.proto")

	cases := []substringCase{
		{"input containing Any clears excluded fields at runtime", true, "srv.ClearSchemaExcluded(&in)"},
		{"output containing Any is masked at runtime", true, "MarshalProtoMasked(resp)"},
	}
	assertSubstrings(t, out, cases)
}

func TestGenerate_BadOneofRequired(t *testing.T) {
	err := runGenerateExpectError(t, "bad_oneof_required.proto")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{
		"BadOneof.Pick",
		"PickRequest.target",
		"every member is masked",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nerror: %v", want, err)
		}
	}
}

func TestGenerate_NoExclusionsEmitsNoClearCall(t *testing.T) {
	out := runGenerate(t, "greeter.proto")
	if strings.Contains(out, "ClearSchemaExcluded") {
		t.Errorf("greeter fixture has no excluded fields; generated output must not call ClearSchemaExcluded")
	}
	if strings.Contains(out, "MarshalProtoMasked") {
		t.Errorf("greeter fixture has no excluded fields; generated output must not call MarshalProtoMasked")
	}
}

func TestGenerate_BadSlimRequired(t *testing.T) {
	cases := []struct {
		name       string
		protoName  string
		wantField  string
		wantMethod string
	}{
		{
			name:       "tool",
			protoName:  "bad_slim_required.proto",
			wantField:  "protomcp.gen.testdata.badslim.v1.BadSlimRequest.id",
			wantMethod: "BadSlim.Describe",
		},
		{
			name:       "prompt only",
			protoName:  "bad_slim_required_prompt.proto",
			wantField:  "protomcp.gen.testdata.badslimprompt.v1.ReviewRequest.id",
			wantMethod: "BadSlimPrompt.Review",
		},
		{
			name:       "resource template only",
			protoName:  "bad_slim_required_resource.proto",
			wantField:  "protomcp.gen.testdata.badslimresource.v1.FetchRequest.tenant",
			wantMethod: "BadSlimResource.Fetch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runGenerateExpectError(t, tc.protoName)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			for _, want := range []string{
				"(protomcp.v1.field_schema).exclude on a required field",
				tc.wantField,
				tc.wantMethod,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q\nerror: %v", want, err)
				}
			}
		})
	}
}

func TestGenerate_SchemaBudget(t *testing.T) {
	t.Run("exceeded", func(t *testing.T) {
		err := runGenerateWithOptionsExpectError(t, "slim.proto", Options{MaxToolSchemaBytes: 50})
		for _, want := range []string{
			"max_tool_schema_bytes=50",
			"(protomcp.v1.field_schema).exclude",
			"Slim.Describe",
		} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want error containing %q, got %v", want, err)
			}
		}
	})
	t.Run("within limit", func(t *testing.T) {
		req := buildGenRequest(t, "slim.proto")
		plugin, err := protogen.Options{}.New(req)
		if err != nil {
			t.Fatalf("protogen.New: %v", err)
		}
		if err := GenerateWithOptions(plugin, Options{MaxToolSchemaBytes: 1 << 20}); err != nil {
			t.Fatalf("GenerateWithOptions: %v", err)
		}
		if resp := plugin.Response(); resp.Error != nil {
			t.Fatalf("plugin error: %s", *resp.Error)
		}
	})
}

func runGenerateWithOptionsExpectError(t *testing.T, protoName string, opts Options) error {
	t.Helper()
	req := buildGenRequest(t, protoName)
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	genErr := GenerateWithOptions(plugin, opts)
	if genErr != nil {
		return genErr
	}
	if resp := plugin.Response(); resp.Error != nil {
		return fmt.Errorf("%s", *resp.Error)
	}
	t.Fatalf("expected generator error for %q, got success", protoName)
	return nil
}

func TestGenerate_BadReadOnlyMutating(t *testing.T) {
	err := runGenerateExpectError(t, "bad_read_only_mutating.proto")
	want := `BadReadOnly.DeleteWidget: protomcp.v1.tool sets read_only: true on an RPC whose name starts with the mutating verb "Delete"`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestGenerate_BadReadOnlyNameOverride(t *testing.T) {
	err := runGenerateExpectError(t, "bad_read_only_name_override.proto")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{
		"BadReadOnlyNameOverride.GetWidget",
		`"delete_widget"`,
		`mutating verb "Delete"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nerror: %v", want, err)
		}
	}
}

func TestGenerate_BadReadOnlyPrefix(t *testing.T) {
	err := runGenerateExpectError(t, "bad_read_only_prefix.proto")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{
		"BadReadOnlyPrefix.GetWidget",
		`"delete_BadReadOnlyPrefix_GetWidget"`,
		`tool_prefix "delete_"`,
		`mutating verb "Delete"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nerror: %v", want, err)
		}
	}
}

func TestGenerate_BadReadOnlyDerivedName(t *testing.T) {
	req := buildGenRequest(t, "read_only_names.proto")
	for _, file := range req.ProtoFile {
		if file.GetName() == "read_only_names.proto" {
			file.Service[0].Name = proto.String("DeleteCatalog")
		}
	}
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	err = Generate(plugin)
	if err == nil {
		if respErr := plugin.Response().Error; respErr != nil {
			err = fmt.Errorf("%s", *respErr)
		} else {
			t.Fatal("want error, got nil")
		}
	}
	for _, want := range []string{
		"DeleteCatalog.GetWidget",
		`"DeleteCatalog_GetWidget"`,
		`mutating verb "Delete"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nerror: %v", want, err)
		}
	}
}

func TestGenerate_ReadOnlyNames(t *testing.T) {
	out := runGenerate(t, "read_only_names.proto")

	cases := []substringCase{
		{"GetWidget generated", true, `"ReadOnlyNames_GetWidget"`},
		{"ListWidgets generated", true, `"ReadOnlyNames_ListWidgets"`},
		{"SettingsInfo not flagged as Set-prefixed", true, `"ReadOnlyNames_SettingsInfo"`},
		{"SetDifference generated via per-RPC lint opt-out", true, `"ReadOnlyNames_SetDifference"`},
		{"read-only hint emitted", true, "&mcp.ToolAnnotations{ReadOnlyHint: true}"},
	}
	assertSubstrings(t, out, cases)
}

func TestGenerate_SlimZeroValueRule(t *testing.T) {
	err := runGenerateExpectError(t, "slim_zero.proto")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{
		"FetchZeroRequest.selector",
		`buf.validate rule "string.min_len"`,
		"cleared zero value always violates",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q\nerror: %v", want, err)
		}
	}
}

func TestMutatingVerbPrefix(t *testing.T) {
	cases := []struct {
		in      string
		wantHit bool
	}{
		{"DeleteWidget", true},
		{"Delete", true},
		{"Set_Value", true},
		{"Update2Widgets", true},
		{"ApplyInstance", true},
		{"GrantAccess", true},
		{"SettingsInfo", false},
		{"Address", false},
		{"Removal", false},
		{"GetWidget", false},
	}
	for _, tc := range cases {
		verb, hit := mutatingVerbPrefix(tc.in)
		if hit != tc.wantHit {
			t.Errorf("mutatingVerbPrefix(%q) hit = %v, want %v", tc.in, hit, tc.wantHit)
		}
		if hit && !strings.HasPrefix(tc.in, verb) {
			t.Errorf("mutatingVerbPrefix(%q) returned verb %q that is not a prefix", tc.in, verb)
		}
		if !hit && verb != "" {
			t.Errorf("mutatingVerbPrefix(%q) returned verb %q without a hit", tc.in, verb)
		}
	}
}

func TestMutatingVerbPrefixFold(t *testing.T) {
	cases := []struct {
		in      string
		wantHit bool
	}{
		{"delete_widget", true},
		{"DeleteWidget", true},
		{"DELETE_WIDGET", true},
		{"delete", true},
		{"update2widgets", true},
		{"deletewidget", false},
		{"settings_info", false},
		{"get_widget", false},
		{"removal", false},
	}
	for _, tc := range cases {
		verb, hit := mutatingVerbPrefixFold(tc.in)
		if hit != tc.wantHit {
			t.Errorf("mutatingVerbPrefixFold(%q) hit = %v, want %v", tc.in, hit, tc.wantHit)
		}
		if hit && verb == "" {
			t.Errorf("mutatingVerbPrefixFold(%q) hit without a verb", tc.in)
		}
	}
}

// TestGenerate_OptionsVariety covers service-level tool_prefix, explicit
// tool name override (with prefix), every combination of hint flags, the
// description-override vs. leading-comment fallback, and the server-
// streaming + explicit PROGRESS stream_mode branch.
func TestGenerate_OptionsVariety(t *testing.T) {
	out := runGenerate(t, "options_variety.proto")

	cases := []substringCase{
		// Service-level prefix is applied to the synthesized name.
		{"prefix + synthesized name", true, `"ns_Prefixed_ReadOnlyOnly"`},
		{"prefix + synthesized name (IdempotentOnly)", true, `"ns_Prefixed_IdempotentOnly"`},
		{"prefix + synthesized name (DestructiveOnly)", true, `"ns_Prefixed_DestructiveOnly"`},
		{"prefix + synthesized name (AllHints)", true, `"ns_Prefixed_AllHints"`},
		{"prefix + synthesized name (NoHints)", true, `"ns_Prefixed_NoHints"`},

		// Explicit name override is used verbatim on top of the prefix. Per
		// the generator, an explicit override is NOT sanitized, the user's
		// string is preserved so dots/slashes survive.
		{"prefix + override preserved verbatim", true, `"ns_custom.name.value"`},
		// The synthesized fallback "ns_Prefixed_Renamed" must NOT appear.
		{"synthesized name does not leak when override set", false, `"ns_Prefixed_Renamed"`},

		// Hint combinations. Each annotation literal must contain exactly
		// the flags that were set, and nothing else.
		{"ReadOnlyOnly has ReadOnlyHint",
			true, "&mcp.ToolAnnotations{ReadOnlyHint: true}"},
		{"IdempotentOnly has IdempotentHint",
			true, "&mcp.ToolAnnotations{IdempotentHint: true}"},
		{"DestructiveOnly has DestructiveHint",
			true, "&mcp.ToolAnnotations{DestructiveHint: protomcp.BoolPtr(true)}"},
		{"AllHints has all three fields",
			true, "&mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: protomcp.BoolPtr(true)}"},

		// Description override vs. leading-comment fallback.
		// The gofmt-aligned output uses two spaces after "Description:" when
		// it lines up with longer neighboring keys (e.g. "OutputSchema:"),
		// so we match on the quoted value alone.
		{"description override is used verbatim",
			true, `"explicit description wins"`},
		{"leading comment is used as fallback description",
			true, `"DescFallback has no description override. The generator must fall back\nto this leading proto comment."`},

		// Server-streaming with explicit PROGRESS mode still emits the
		// streaming template branch.
		{"ProgressStream emits NotifyProgress", true, "NotifyProgress"},
		{"ProgressStream is a tool", true, `"ns_Prefixed_ProgressStream"`},
	}
	assertSubstrings(t, out, cases)

	// NoHints has every hint flag clear. The ToolAnnotations struct literal
	// must NOT be emitted for it at all. We slice the output to the NoHints
	// tool block (the region between the "ns_Prefixed_NoHints" Name line
	// and the next AddTool call) and assert Annotations: never appears.
	assertNoAnnotationsInBlock(t, out, `"ns_Prefixed_NoHints"`)
}

// TestGenerate_MultiService asserts both services in a multi-service proto
// produce their own Register<X>MCPTools function, and that a bidi-streaming
// RPC is skipped with the bidi-specific comment (distinct from the
// client-streaming comment).
func TestGenerate_MultiService(t *testing.T) {
	out := runGenerate(t, "multi_service.proto")

	cases := []substringCase{
		{"Alpha register function", true, "func RegisterAlphaMCPTools("},
		{"Beta register function", true, "func RegisterBetaMCPTools("},
		{"Alpha tool", true, `"Alpha_Ping"`},
		{"Beta tool", true, `"Beta_Echo"`},
		{"unannotated bidi Duplex is not registered", false, `"Beta_Duplex"`},
		{"no skip comments", false, "protoc-gen-mcp: skipping"},
	}
	assertSubstrings(t, out, cases)
}

// TestGenerate_Prompts covers the prompt annotation codegen path:
//   - RegisterPromptSvcMCPPrompts register function is emitted
//   - prompt name, title, description, and arguments appear
//   - srv.MustAddPrompt is the registration call (not a tool registration),
//     routed through protomcp so the prompt name is claimed
//   - enum argument completions are registered via
//     RegisterPromptArgCompletions (enum value names minus _UNSPECIFIED)
//   - buf.validate.string.in values are registered as completions too
//   - FinishPromptGet / PromptChain are the runtime hooks
func TestGenerate_Prompts(t *testing.T) {
	out := runGenerate(t, "prompts.proto")

	cases := []substringCase{
		{"prompts register function", true, "RegisterPromptSvcMCPPrompts"},
		{"review_item prompt name", true, `"review_item"`},
		{"prompt title emitted", true, `"Review an item"`},
		{"prompt description emitted", true, `"Ask the LLM to review a single item."`},
		{"prompt required arg", true, `Required: true`},
		{"claiming AddPrompt", true, "srv.MustAddPrompt"},
		{"prompt registration does not bypass the claim", false, "srv.SDK().AddPrompt"},
		{"no tool registration for prompt-only svc", false, "MustAddTool"},
		{"prompt final handler uses PromptChain", true, "srv.PromptChain(final)"},
		{"prompt handler uses FinishPromptGet", true, "srv.FinishPromptGet"},
		{"mustache render call", true, "mustache.Render"},
		{"enum completions registered", true, `RegisterPromptArgCompletions("review_item", "priority"`},
		{"enum value names", true, `"PRIORITY_LOW"`},
		{"unspecified excluded", false, `"PRIORITY_UNSPECIFIED"`},
		{"string.in completions registered", true, `RegisterPromptArgCompletions("PromptSvc_CategorySelect", "category"`},
		{"string.in values", true, `"alpha"`},
	}
	assertSubstrings(t, out, cases)
}

func TestGenerate_PromptRequiredness(t *testing.T) {
	out := runGenerate(t, "prompt_requiredness.proto")
	assertSubstrings(t, out, []substringCase{
		{"prompt args decoded through protojson", true, "srv.UnmarshalProto(payload, in)"},
		{"optional string is not assigned directly", false, "in.RequiredIgnoreZeroWithPresence = v"},
	})
	cases := []struct {
		name     string
		required bool
	}{
		{"apiRequired", true},
		{"protovalidateRequired", true},
		{"requiredIgnoreAlways", false},
		{"requiredIgnoreZero", false},
		{"requiredIgnoreZeroWithPresence", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := `"` + tc.name + `"`
			start := strings.Index(out, marker)
			if start < 0 {
				t.Fatalf("prompt argument %q not found\n--- file ---\n%s", tc.name, out)
			}
			block := out[start:]
			if end := strings.Index(block, "},"); end >= 0 {
				block = block[:end]
			}
			if got := strings.Contains(block, "Required: true"); got != tc.required {
				t.Errorf("prompt argument %q required = %v, want %v\n--- block ---\n%s", tc.name, got, tc.required, block)
			}
		})
	}
}

func TestGenerate_PromptOutputOnlyArgumentOmitted(t *testing.T) {
	req := buildGenRequest(t, "prompt_requiredness.proto")
	for _, file := range req.ProtoFile {
		if file.GetName() != "prompt_requiredness.proto" {
			continue
		}
		field := file.MessageType[0].Field[2]
		proto.SetExtension(field.Options, annotations.E_FieldBehavior, []annotations.FieldBehavior{
			annotations.FieldBehavior_OUTPUT_ONLY,
		})
	}
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	if err := Generate(plugin); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	resp := plugin.Response()
	if resp.Error != nil {
		t.Fatalf("plugin error: %s", *resp.Error)
	}
	out := resp.File[0].GetContent()
	if strings.Contains(out, `"requiredIgnoreAlways"`) {
		t.Fatalf("OUTPUT_ONLY request field was exposed as a prompt argument\n--- file ---\n%s", out)
	}
}

// TestGenerate_BadPromptStreams_Errors asserts the generator returns a
// clear error when a streaming RPC is annotated with protomcp.v1.prompt.
func TestGenerate_BadPromptStreams_Errors(t *testing.T) {
	err := runGenerateExpectError(t, "bad_prompt_streams.proto")
	want := "BadPromptStream.Watch: server-streaming RPCs cannot be exposed as MCP prompts"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

// TestGenerate_BadPromptTemplate_Errors asserts that Mustache section
// syntax (and by extension inverted-section + partial syntax) fails
// codegen with an actionable error.
func TestGenerate_BadPromptTemplate_Errors(t *testing.T) {
	err := runGenerateExpectError(t, "bad_prompt_template.proto")
	want := "sections ({{#items}}) are not supported"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

// TestGenerate_NoTools asserts that a proto with no annotated methods
// produces no generated file at all.
func TestGenerate_NoTools(t *testing.T) {
	req := buildGenRequest(t, "no_tools.proto")
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	if err := Generate(plugin); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	resp := plugin.Response()
	if resp.Error != nil {
		t.Fatalf("plugin error: %s", *resp.Error)
	}
	if got := len(resp.File); got != 0 {
		t.Fatalf("expected 0 generated files for a proto with no annotated "+
			"methods, got %d:\n%s", got, resp.File[0].GetContent())
	}
}

// TestSanitizeToolName covers the tool-name sanitizer directly. Proto
// service names are constrained to identifier characters, so dots and
// slashes can only leak into a synthesized tool name via a malformed or
// hand-crafted descriptor, but the sanitizer is defensive code the rest
// of the generator relies on, so we verify it behaves as advertised.
func TestSanitizeToolName(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"Greeter_SayHello", false},
		{"ns.v1.Greeter_SayHello", false}, // dots are legal per MCP spec
		{"Greeter-SayHello", false},       // dashes too
		{"a/b/c", true},                   // slash is not in [a-zA-Z0-9_.-]
		{"has a space", true},
		{"", true},
		{strings.Repeat("x", 129), true},
	}
	for _, tc := range cases {
		err := validateMCPIdentifier(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateMCPIdentifier(%q) = %v, wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

// --- test helpers ------------------------------------------------------

// substringCase is the shared shape for table-driven substring assertions
// on generator output: contains=true means "the output must contain needle",
// contains=false means "the output must NOT contain needle".
type substringCase struct {
	name     string
	contains bool
	needle   string
}

// runGenerate drives the generator against the named proto (which must be
// one of the files packed into fixtures.binpb) and returns the single
// generated file's content. It fails the test if anything other than one
// file is emitted.
func runGenerate(t *testing.T, protoName string) string {
	t.Helper()
	req := buildGenRequest(t, protoName)
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	if err := Generate(plugin); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	resp := plugin.Response()
	if resp.Error != nil {
		t.Fatalf("plugin error: %s", *resp.Error)
	}
	if got := len(resp.File); got != 1 {
		t.Fatalf("expected 1 generated file, got %d", got)
	}
	wantFilename := strings.TrimSuffix(protoName, ".proto") + ".mcp.pb.go"
	if !strings.HasSuffix(resp.File[0].GetName(), wantFilename) {
		t.Errorf("output filename = %q, want suffix %q",
			resp.File[0].GetName(), wantFilename)
	}
	out := resp.File[0].GetContent()
	// Every successful generation must be compilable, and the cheapest
	// compile error to leak past substring assertions is an unused
	// import: protogen emits one import line per QualifiedGoIdent call
	// and never prunes, so qualifying an identifier the templates end up
	// not rendering breaks the user's build.
	assertNoUnusedImports(t, protoName, out)
	return out
}

// runGenerateExpectError drives the generator against protoName and
// returns the error it produced. It fails the test if the generator
// unexpectedly succeeded.
func runGenerateExpectError(t *testing.T, protoName string) error {
	t.Helper()
	req := buildGenRequest(t, protoName)
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	genErr := Generate(plugin)
	if genErr != nil {
		return genErr
	}
	// If Generate returned nil, the plugin may have surfaced the error via
	// its Response().Error field instead, check there too.
	if resp := plugin.Response(); resp.Error != nil {
		return fmt.Errorf("%s", *resp.Error)
	}
	t.Fatalf("expected generator error for %q, got success", protoName)
	return nil
}

// assertNoUnusedImports fails the test if the generated file imports a
// package it never references. `go build` would reject such a file, but
// the generator tests only ever see the source as a string, so the check
// is done over the AST: collect the qualifier of every selector
// expression, then require each named import to appear among them. Blank
// and dot imports are exempt (they have no qualifier by construction).
func assertNoUnusedImports(t *testing.T, protoName, out string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "generated.go", out, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse generated output for %s: %v", protoName, err)
	}
	used := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, isIdent := sel.X.(*ast.Ident); isIdent {
				used[id.Name] = true
			}
		}
		return true
	})
	for _, imp := range file.Imports {
		path, uErr := strconv.Unquote(imp.Path.Value)
		if uErr != nil {
			t.Fatalf("unquote import path %s in %s: %v", imp.Path.Value, protoName, uErr)
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
			name = imp.Name.Name
		}
		if !used[name] {
			t.Errorf("generated file for %s imports %q as %q but never "+
				"references it; the file would not compile\n--- file ---\n%s",
				protoName, path, name, out)
		}
	}
}

// assertSubstrings runs a table of substring presence/absence checks
// against out, dumping the full file on failure so the failing assertion
// has enough context to debug.
func assertSubstrings(t *testing.T, out string, cases []substringCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Contains(out, tc.needle)
			if got != tc.contains {
				t.Errorf("contains(%q) = %v, want %v\n--- file ---\n%s",
					tc.needle, got, tc.contains, out)
			}
		})
	}
}

// assertNoAnnotationsInBlock slices out the registration block beginning
// at toolNameMarker and extending to the next AddTool call, then fails
// the test if "Annotations:" appears inside that window. It lets us
// assert the "no hints set -> no Annotations field" contract without
// being fooled by neighboring tools' literals.
func assertNoAnnotationsInBlock(t *testing.T, out, toolNameMarker string) {
	t.Helper()
	idx := strings.Index(out, toolNameMarker)
	if idx < 0 {
		t.Fatalf("marker %q not found in generated output:\n%s", toolNameMarker, out)
	}
	block := out[idx:]
	if next := strings.Index(block[1:], "AddTool("); next >= 0 {
		block = block[:next+1]
	}
	if strings.Contains(block, "Annotations:") {
		t.Errorf("tool block at %q must not emit an Annotations field, but got:\n%s",
			toolNameMarker, block)
	}
}

// buildGenRequest constructs a CodeGeneratorRequest from the precompiled
// fixtures.binpb FileDescriptorSet. The first argument names the file to
// generate; all transitively imported files are included as context so
// protogen can resolve cross-file references.
func buildGenRequest(t *testing.T, target string) *pluginpb.CodeGeneratorRequest {
	t.Helper()

	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(fixturesBin, &fds); err != nil {
		t.Fatalf("unmarshal fixtures.binpb: %v", err)
	}

	// Sanity: target must exist in the set.
	found := false
	for _, f := range fds.File {
		if f.GetName() == target {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("target proto %q not present in fixtures.binpb; regenerate it with "+
			"protoc --include_source_info --include_imports", target)
	}

	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{target},
		// ProtoFile must include every file transitively referenced, in
		// dependency order. protoc's --include_imports already orders deps
		// before dependents, so we pass the set through unchanged.
		ProtoFile: fds.File,
		CompilerVersion: &pluginpb.Version{
			Major: proto.Int32(3),
			Minor: proto.Int32(21),
			Patch: proto.Int32(12),
		},
	}
}
