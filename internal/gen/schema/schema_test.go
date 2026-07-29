package schema

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/akuity/protomcp/internal/gen/schema/testdata"
)

// descByName looks up a message descriptor registered in the global protoregistry.
// The testdata package is imported for its side effects (registration).
func descByName(t *testing.T, name string) protoreflect.MessageDescriptor {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(name))
	if err != nil {
		t.Fatalf("descriptor %s not found: %v", name, err)
	}
	return mt.Descriptor()
}

// jsonRound marshals a schema to JSON and back so we can compare via reflect.DeepEqual
// without worrying about map insertion order.
func jsonRound(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// get returns nested map keys as a single lookup, e.g. get(s, "properties", "i64", "type").
func get(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	cur := any(m)
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: expected map at %q, got %T", path, p, cur)
		}
		cur = mm[p]
	}
	return cur
}

func requiredSet(m map[string]any) map[string]bool {
	got := map[string]bool{}
	if r, ok := m["required"].([]any); ok {
		for _, v := range r {
			got[v.(string)] = true
		}
	}
	return got
}

// propertyNames returns the sorted set of property names on an object schema.
func propertyNames(m map[string]any) []string {
	props, _ := m["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestScalars(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Scalars")
	schema := jsonRound(t, ForInput(md, Options{}))

	// int64 and uint64 must render as string per protojson.
	if got := get(t, schema, "properties", "i64", "type"); got != "string" {
		t.Errorf("int64 type: want string, got %v", got)
	}
	if got := get(t, schema, "properties", "u64", "type"); got != "string" {
		t.Errorf("uint64 type: want string, got %v", got)
	}
	// int32/uint32 remain integer.
	if got := get(t, schema, "properties", "i32", "type"); got != "integer" {
		t.Errorf("int32 type: want integer, got %v", got)
	}
	// bytes carries contentEncoding: base64.
	if got := get(t, schema, "properties", "raw", "contentEncoding"); got != "base64" {
		t.Errorf("bytes contentEncoding: want base64, got %v", got)
	}
}

func TestEnumPlain(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Enums")
	schema := jsonRound(t, ForInput(md, Options{}))

	plain := get(t, schema, "properties", "plain").(map[string]any)
	if plain["type"] != "string" {
		t.Errorf("enum type: want string, got %v", plain["type"])
	}
	vals := plain["enum"].([]any)
	want := []string{"STATUS_UNSPECIFIED", "STATUS_OK", "STATUS_ERROR"}
	got := make([]string, len(vals))
	for i, v := range vals {
		got[i] = v.(string)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("enum values: want %v, got %v", want, got)
	}
}

func TestEnumConstraints(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Enums")
	schema := jsonRound(t, ForInput(md, Options{}))

	// defined_only + UNSPECIFIED still allowed (we don't auto-exclude UNSPECIFIED).
	// Nothing to compare strictly, just ensure the enum was preserved.
	if _, ok := get(t, schema, "properties", "definedOnly").(map[string]any)["enum"]; !ok {
		t.Error("defined_only: expected enum values")
	}

	// in: [1, 2] → STATUS_OK, STATUS_ERROR
	onlyIn := get(t, schema, "properties", "onlyIn").(map[string]any)
	enumVals := onlyIn["enum"].([]any)
	if len(enumVals) != 2 || enumVals[0] != "STATUS_OK" || enumVals[1] != "STATUS_ERROR" {
		t.Errorf("only_in enum: got %v", enumVals)
	}

	// const: 1 → single-value enum
	constant := get(t, schema, "properties", "constant").(map[string]any)
	cVals := constant["enum"].([]any)
	if len(cVals) != 1 || cVals[0] != "STATUS_OK" {
		t.Errorf("constant enum: got %v", cVals)
	}
}

func TestRequiredAndOutputOnly(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Required")
	in := jsonRound(t, ForInput(md, Options{}))

	// OUTPUT_ONLY field must be absent from input properties.
	if _, ok := in["properties"].(map[string]any)["serverComputed"]; ok {
		t.Error("OUTPUT_ONLY field leaked into input schema")
	}
	// The other three fields must be present.
	if _, ok := in["properties"].(map[string]any)["apiRequired"]; !ok {
		t.Error("api_required missing from input")
	}

	req := requiredSet(in)
	if !req["apiRequired"] {
		t.Error("api_required not in required[]")
	}
	if !req["protovalidateRequired"] {
		t.Error("protovalidate_required not in required[]")
	}
	if req["optionalField"] {
		t.Error("optional_field wrongly listed as required")
	}

	// Output schema includes server_computed (no stripping).
	out := jsonRound(t, ForOutput(md, Options{}))
	if _, ok := out["properties"].(map[string]any)["serverComputed"]; !ok {
		t.Error("server_computed missing from output schema")
	}
}

func TestStringConstraints(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Strings")
	s := jsonRound(t, ForInput(md, Options{}))

	if f := get(t, s, "properties", "uuid", "format"); f != "uuid" {
		t.Errorf("uuid format: %v", f)
	}
	if f := get(t, s, "properties", "email", "format"); f != "email" {
		t.Errorf("email format: %v", f)
	}
	if p := get(t, s, "properties", "pat", "pattern"); p != "^[a-z]+$" {
		t.Errorf("pattern: %v", p)
	}
	if mn := get(t, s, "properties", "ranged", "minLength"); mn != float64(3) {
		t.Errorf("minLength: %v (%T)", mn, mn)
	}
	if mx := get(t, s, "properties", "ranged", "maxLength"); mx != float64(10) {
		t.Errorf("maxLength: %v", mx)
	}
}

func TestNumericConstraints(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Numeric")
	s := jsonRound(t, ForInput(md, Options{}))

	// gt/lt → exclusiveMinimum/exclusiveMaximum
	if v := get(t, s, "properties", "i32Open", "exclusiveMinimum"); v != float64(0) {
		t.Errorf("i32_open exclusiveMinimum: %v", v)
	}
	if v := get(t, s, "properties", "i32Open", "exclusiveMaximum"); v != float64(100) {
		t.Errorf("i32_open exclusiveMaximum: %v", v)
	}
	// gte/lte → minimum/maximum
	if v := get(t, s, "properties", "i32Closed", "minimum"); v != float64(0) {
		t.Errorf("i32_closed minimum: %v", v)
	}
	if v := get(t, s, "properties", "i32Closed", "maximum"); v != float64(100) {
		t.Errorf("i32_closed maximum: %v", v)
	}
	// float closed
	if v := get(t, s, "properties", "fClosed", "maximum"); v != float64(1) {
		t.Errorf("f_closed maximum: %v", v)
	}
	// double open → exclusive bounds
	if v := get(t, s, "properties", "dOpen", "exclusiveMinimum"); v != float64(0) {
		t.Errorf("d_open exclusiveMinimum: %v", v)
	}
	if v := get(t, s, "properties", "dOpen", "exclusiveMaximum"); v != float64(1) {
		t.Errorf("d_open exclusiveMaximum: %v", v)
	}
}

func TestRepeatedRules(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Listy")
	s := jsonRound(t, ForInput(md, Options{}))

	tags := get(t, s, "properties", "tags").(map[string]any)
	if tags["type"] != "array" {
		t.Errorf("tags.type: %v", tags["type"])
	}
	if tags["minItems"] != float64(1) {
		t.Errorf("tags.minItems: %v", tags["minItems"])
	}
	if tags["maxItems"] != float64(5) {
		t.Errorf("tags.maxItems: %v", tags["maxItems"])
	}
	if tags["uniqueItems"] != true {
		t.Errorf("tags.uniqueItems: %v", tags["uniqueItems"])
	}

	// Item-level constraints reach through GetRepeated().GetItems().
	bounded := get(t, s, "properties", "boundedItems").(map[string]any)
	items := bounded["items"].(map[string]any)
	if items["minLength"] != float64(2) {
		t.Errorf("bounded_items item minLength: %v", items["minLength"])
	}
}

func TestMapConstraints(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Mapy")
	s := jsonRound(t, ForInput(md, Options{}))

	counts := get(t, s, "properties", "counts").(map[string]any)
	if counts["type"] != "object" {
		t.Errorf("counts.type: %v", counts["type"])
	}
	pn := counts["propertyNames"].(map[string]any)
	if pn["minLength"] != float64(1) {
		t.Errorf("counts key minLength: %v", pn["minLength"])
	}
	ap := counts["additionalProperties"].(map[string]any)
	if ap["minimum"] != float64(0) {
		t.Errorf("counts value minimum: %v", ap["minimum"])
	}

	// int64 map key → numeric pattern on propertyNames.
	byID := get(t, s, "properties", "byId").(map[string]any)
	keyConstraints := byID["propertyNames"].(map[string]any)
	if _, ok := keyConstraints["pattern"]; !ok {
		t.Errorf("int64 map key: expected pattern, got %v", keyConstraints)
	}
}

func TestWellKnownTypes(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.WellKnown")
	s := jsonRound(t, ForInput(md, Options{}))

	cases := map[string]struct {
		want map[string]any
	}{
		"ts":  {map[string]any{"format": "date-time"}},
		"dur": {map[string]any{"pattern": `^-?[0-9]+(\.[0-9]+)?s$`}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			prop := get(t, s, "properties", name).(map[string]any)
			for k, v := range tc.want {
				if prop[k] != v {
					t.Errorf("%s[%s]: want %v, got %v", name, k, v, prop[k])
				}
			}
		})
	}

	// Wrapper types: nullable primitive.
	sv := get(t, s, "properties", "sv").(map[string]any)
	types := sv["type"].([]any)
	if len(types) != 2 || types[0] != "string" || types[1] != "null" {
		t.Errorf("StringValue type: want [string null], got %v", types)
	}
	// Int64Value renders as nullable string (not number).
	lv := get(t, s, "properties", "i64v").(map[string]any)
	lvTypes := lv["type"].([]any)
	if lvTypes[0] != "string" {
		t.Errorf("Int64Value primary type: want string, got %v", lvTypes[0])
	}
}

func TestOneofs(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Oneofs")
	s := jsonRound(t, ForInput(md, Options{}))

	// Two real oneofs (choice, fallback) produce two allOf entries in declaration order.
	allOf, ok := s["allOf"].([]any)
	if !ok || len(allOf) != 2 {
		t.Fatalf("expected two allOf entries (choice, fallback), got %d: %v", len(allOf), s["allOf"])
	}
	choiceOneOf := allOf[0].(map[string]any)["oneOf"].([]any)
	if len(choiceOneOf) != 5 {
		t.Errorf("choice oneOf: want 5 branches (text,count,nested,label + no-arm), got %d", len(choiceOneOf))
	}
	fallbackOneOf := allOf[1].(map[string]any)["oneOf"].([]any)
	if len(fallbackOneOf) != 3 {
		t.Errorf("fallback oneOf: want 3 branches (reason,silent + no-arm), got %d", len(fallbackOneOf))
	}
	// The last branch of each group accepts "no arm set" and rejects any set arm.
	noArm := choiceOneOf[len(choiceOneOf)-1].(map[string]any)
	notAnyOf, ok := noArm["not"].(map[string]any)["anyOf"].([]any)
	if !ok || len(notAnyOf) != 4 {
		t.Fatalf("choice no-arm branch: want not.anyOf with 4 entries, got %v", noArm)
	}

	// Arm value schemas live in top-level properties (branches are
	// presence-only), so a wrong-typed arm fails even when another arm
	// carries the oneOf match.
	props := s["properties"].(map[string]any)
	for _, name := range []string{"text", "count", "nested", "label", "reason", "silent"} {
		if _, ok := props[name]; !ok {
			t.Errorf("oneof arm %s missing from top-level properties", name)
		}
		if requiredSet(s)[name] {
			t.Errorf("oneof arm %s wrongly in top-level required", name)
		}
	}

	// Synthetic oneofs (proto3 `optional`) must NOT appear in allOf; the fields
	// should be regular optional properties.
	for _, name := range []string{"maybeNote", "maybeCount"} {
		if _, ok := props[name]; !ok {
			t.Errorf("%s should be a regular optional property (synthetic oneof)", name)
		}
		if requiredSet(s)[name] {
			t.Errorf("%s wrongly required", name)
		}
	}
}

func TestHasExclusions_Any(t *testing.T) {
	if !HasExclusions(descByName(t, "protomcp.testdata.v1.WellKnown")) {
		t.Error("HasExclusions(WellKnown) = false; a reachable google.protobuf.Any must count as an exclusion")
	}
	if HasExclusions(descByName(t, "protomcp.testdata.v1.Scalars")) {
		t.Error("HasExclusions(Scalars) = true, want false")
	}
}

func TestRequiredOneof_NoEmptyBranch(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.RequiredOneof")
	s := jsonRound(t, ForInput(md, Options{}))

	allOf, ok := s["allOf"].([]any)
	if !ok || len(allOf) != 1 {
		t.Fatalf("expected one allOf entry, got %v", s["allOf"])
	}
	oneOf := allOf[0].(map[string]any)["oneOf"].([]any)
	if len(oneOf) != 2 {
		t.Fatalf("required oneof: want 2 branches (id, name) and no no-arm branch, got %d: %v", len(oneOf), oneOf)
	}
	for _, b := range oneOf {
		if _, hasNot := b.(map[string]any)["not"]; hasNot {
			t.Errorf("required oneof must not carry a no-arm branch: %v", b)
		}
	}

	raw, err := json.Marshal(ForInput(md, Options{}))
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	sch := &jsonschema.Schema{}
	if uErr := json.Unmarshal(raw, sch); uErr != nil {
		t.Fatalf("unmarshal into jsonschema.Schema: %v", uErr)
	}
	resolved, err := sch.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}
	cases := []struct {
		name     string
		instance map[string]any
		valid    bool
	}{
		{"no arm rejected", map[string]any{}, false},
		{"no arm rejected even with other fields", map[string]any{"note": "x"}, false},
		{"one arm validates", map[string]any{"id": "x"}, true},
		{"other arm validates", map[string]any{"name": "y"}, true},
		{"two arms rejected", map[string]any{"id": "x", "name": "y"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resolved.Validate(tc.instance)
			if tc.valid && err != nil {
				t.Errorf("want valid, got %v", err)
			}
			if !tc.valid && err == nil {
				t.Errorf("want validation failure, got none")
			}
		})
	}
}

func TestRequiredOneofPartial_InputRequiresVisibleArm(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.RequiredOneofPartial")

	in := jsonRound(t, ForInput(md, Options{}))
	inOneOf := in["allOf"].([]any)[0].(map[string]any)["oneOf"].([]any)
	if len(inOneOf) != 1 {
		t.Fatalf("input oneOf: want 1 branch (visible only), got %d: %v", len(inOneOf), inOneOf)
	}
	for _, b := range inOneOf {
		if _, hasNot := b.(map[string]any)["not"]; hasNot {
			t.Errorf("input schema of a required oneof must not accept the no-arm case: %v", inOneOf)
		}
	}

	raw, err := json.Marshal(ForInput(md, Options{}))
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	sch := &jsonschema.Schema{}
	if uErr := json.Unmarshal(raw, sch); uErr != nil {
		t.Fatalf("unmarshal into jsonschema.Schema: %v", uErr)
	}
	resolved, err := sch.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}
	if vErr := resolved.Validate(map[string]any{}); vErr == nil {
		t.Error("empty input validated; a required oneof with a visible arm must demand that arm")
	}
	if vErr := resolved.Validate(map[string]any{"visible": "x"}); vErr != nil {
		t.Errorf("visible arm rejected: %v", vErr)
	}
	if vErr := resolved.Validate(map[string]any{"hidden": "y"}); vErr == nil {
		t.Error("masked arm validated; hidden is OUTPUT_ONLY and cannot satisfy the oneof")
	}

	out := jsonRound(t, ForOutput(md, Options{}))
	outOneOf := out["allOf"].([]any)[0].(map[string]any)["oneOf"].([]any)
	if len(outOneOf) != 2 {
		t.Fatalf("output oneOf: want 2 branches (visible, hidden), got %d: %v", len(outOneOf), outOneOf)
	}
	for _, b := range outOneOf {
		if _, hasNot := b.(map[string]any)["not"]; hasNot {
			t.Errorf("output schema sees every arm; no-arm branch must be dropped: %v", outOneOf)
		}
	}
}

func TestRequiredOneofPartial_FilteredOutputKeepsEmptyBranch(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.RequiredOneofPartial")

	s := jsonRound(t, messageSchema(md, Options{}, nil, func(fd protoreflect.FieldDescriptor) bool {
		return fd.Name() != "hidden"
	}, false))
	oneOf := s["allOf"].([]any)[0].(map[string]any)["oneOf"].([]any)
	if len(oneOf) != 2 {
		t.Fatalf("output oneOf: want 2 branches (visible + no-arm), got %d: %v", len(oneOf), oneOf)
	}
	if _, hasNot := oneOf[1].(map[string]any)["not"]; !hasNot {
		t.Errorf("output schema with a filtered arm must keep the no-arm branch: %v", oneOf)
	}
}

func TestOneofArmRequired_ArmInTopLevelRequired(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.OneofArmRequired")
	s := jsonRound(t, ForInput(md, Options{}))
	if !requiredSet(s)["id"] {
		t.Fatalf("required oneof arm id missing from top-level required: %v", s["required"])
	}
	if requiredSet(s)["name"] {
		t.Fatalf("non-required arm name wrongly in top-level required: %v", s["required"])
	}

	raw, err := json.Marshal(ForInput(md, Options{}))
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	sch := &jsonschema.Schema{}
	if uErr := json.Unmarshal(raw, sch); uErr != nil {
		t.Fatalf("unmarshal into jsonschema.Schema: %v", uErr)
	}
	resolved, err := sch.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}
	cases := []struct {
		name     string
		instance map[string]any
		valid    bool
	}{
		{"no arm rejected", map[string]any{}, false},
		{"other arm alone rejected", map[string]any{"name": "x"}, false},
		{"required arm validates", map[string]any{"id": "x"}, true},
		{"both arms rejected", map[string]any{"id": "x", "name": "y"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resolved.Validate(tc.instance)
			if tc.valid && err != nil {
				t.Errorf("want valid, got %v", err)
			}
			if !tc.valid && err == nil {
				t.Errorf("want validation failure, got none")
			}
		})
	}
}

func TestZeroValueViolation(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.ZeroBreakers")
	cases := map[string]string{
		"min_len_s":        "string.min_len",
		"min_items_r":      "repeated.min_items",
		"min_pairs_m":      "map.min_pairs",
		"gt_i":             "int32.gt",
		"gt_u":             "uint64.gt",
		"lte_neg":          "double.lte",
		"const_b":          "bool.const",
		"pattern_s":        "string.pattern",
		"email_s":          "string.<format>",
		"enum_in":          "enum.in",
		"ok_pattern":       "",
		"ok_max":           "",
		"ok_presence":      "",
		"ok_ignore_always": "",
		"ok_ignore_zero":   "",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			fd := md.Fields().ByName(protoreflect.Name(name))
			if fd == nil {
				t.Fatalf("field %s not found", name)
			}
			rule, bad := zeroValueViolation(fd)
			if want == "" && bad {
				t.Errorf("zeroValueViolation = %q, want none", rule)
			}
			if want != "" && (!bad || rule != want) {
				t.Errorf("zeroValueViolation = (%q, %v), want (%q, true)", rule, bad, want)
			}
		})
	}
	if isRequired(md.Fields().ByName("req_ignore_always")) {
		t.Error("required=true with ignore=IGNORE_ALWAYS must not count as required")
	}
}

func TestRequiredOneofAllMasked_InputErrors(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.RequiredOneofAllMasked")
	if _, err := ForInputE(md, Options{}); err == nil || !strings.Contains(err.Error(), "every member is masked") {
		t.Fatalf("ForInputE: want required-oneof masking error, got %v", err)
	}
	if _, err := ForOutputE(md, Options{}); err != nil {
		t.Fatalf("ForOutputE: %v", err)
	}
}

func TestOneofs_ZeroOrOneValidation(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Oneofs")
	raw, err := json.Marshal(ForOutput(md, Options{}))
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	sch := &jsonschema.Schema{}
	if uErr := json.Unmarshal(raw, sch); uErr != nil {
		t.Fatalf("unmarshal into jsonschema.Schema: %v", uErr)
	}
	resolved, err := sch.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}

	cases := []struct {
		name     string
		instance map[string]any
		valid    bool
	}{
		{"no arm set validates", map[string]any{}, true},
		{"one arm set validates", map[string]any{"text": "x"}, true},
		{"one arm per oneof validates", map[string]any{"count": float64(2), "reason": "r"}, true},
		{"two arms of the same oneof rejected", map[string]any{"text": "x", "count": float64(2)}, false},
		{"second arm rejected even when wrong-typed", map[string]any{"text": "x", "count": "wrong-type"}, false},
		{"single wrong-typed arm rejected", map[string]any{"count": "wrong-type"}, false},
		{"second oneof over-set rejected even when first is valid", map[string]any{"text": "x", "reason": "r", "silent": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resolved.Validate(tc.instance)
			if tc.valid && err != nil {
				t.Errorf("want valid, got %v", err)
			}
			if !tc.valid && err == nil {
				t.Errorf("want validation failure, got none")
			}
		})
	}
}

func TestRecursionCap(t *testing.T) {
	md := descByName(t, "protomcp.testdata.v1.Recursive")
	s := jsonRound(t, ForInput(md, Options{MaxRecursionDepth: 2}))

	// First expansion has "name" and "child".
	if names := propertyNames(s); !reflect.DeepEqual(names, []string{"child", "name"}) {
		t.Fatalf("top-level properties: %v", names)
	}
	// Second level expansion has "name" and "child" (placeholder).
	child1 := get(t, s, "properties", "child").(map[string]any)
	if names := propertyNames(child1); !reflect.DeepEqual(names, []string{"child", "name"}) {
		t.Fatalf("child1 properties: %v", names)
	}
	// Third level = placeholder string with the JSON-encoded hint.
	child2 := get(t, child1, "properties", "child").(map[string]any)
	if child2["type"] != "string" {
		t.Errorf("recursion cap: expected string placeholder at max depth, got %v", child2["type"])
	}
}

func TestCleanComment(t *testing.T) {
	in := "  buf:lint: ignore_unused\n  Real description.\n  @ignore-comment\n  Another line."
	got := CleanComment(in)
	want := "Real description.\nAnother line."
	if got != want {
		t.Errorf("CleanComment: want %q, got %q", want, got)
	}
}

// Sanity: ensure ForInput output is JSON-serializable.
func TestSchemaIsJSONSerializable(t *testing.T) {
	for _, name := range []string{"Scalars", "Enums", "Required", "Strings", "Numeric", "Listy", "Mapy", "WellKnown", "Oneofs", "Recursive"} {
		t.Run(name, func(t *testing.T) {
			md := descByName(t, "protomcp.testdata.v1."+name)
			s := ForInput(md, Options{})
			if _, err := json.Marshal(s); err != nil {
				t.Fatalf("marshal: %v", err)
			}
		})
	}
	_ = proto.Message(nil) // silence unused import if it happens
}
