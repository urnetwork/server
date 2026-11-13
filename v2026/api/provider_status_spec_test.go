package api

// GET /network/provider-status in the published spec (connect/api/bringyour.yml):
// its query parameters, its conditional requests (If-None-Match, ETag and
// 304), both forms of its answer field for field in both directions, and the
// reasons and reason codes a status gives. Pure: no database or redis.

import (
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The query parameters the spec documents for path and method, by name.
func specQueryParameters(spec *specDoc, method string, path string) map[string]map[string]any {
	parameters := map[string]map[string]any{}
	operation := asMap(asMap(spec.paths[path])[method])
	for _, parameterAny := range asSlice(operation["parameters"]) {
		parameter := asMap(parameterAny)
		if parameter["in"] == "query" {
			parameters[parameter["name"].(string)] = parameter
		}
	}
	return parameters
}

// The json names of goType that the spec schema does not document, and the
// same for every object it nests, by dotted path.
func undocumentedJsonFields(spec *specDoc, prefix string, specSchema map[string]any, goType reflect.Type) []string {
	missing := []string{}
	properties := spec.properties(specSchema)
	for name, fieldType := range goFields(goType) {
		property, ok := properties[name]
		if !ok {
			missing = append(missing, prefix+name)
			continue
		}
		resolved := spec.resolve(asMap(property))
		elemType := deref(fieldType)
		switch {
		case elemType.Kind() == reflect.Struct && !marshalsToString(elemType):
			missing = append(missing, undocumentedJsonFields(spec, prefix+name+".", resolved, elemType)...)
		case elemType.Kind() == reflect.Slice:
			if itemType := deref(elemType.Elem()); itemType.Kind() == reflect.Struct && !marshalsToString(itemType) {
				missing = append(missing, undocumentedJsonFields(spec, prefix+name+"[].", spec.resolve(asMap(resolved["items"])), itemType)...)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// The query takes an optional client_id and an optional format, full or
// compact, and the answer is one of the two result forms.
func TestProviderStatusSpecDocumentsTheQuery(t *testing.T) {
	spec := loadSpec(t)
	parameters := specQueryParameters(spec, "get", "/network/provider-status")

	clientId, ok := parameters["client_id"]
	if !ok {
		t.Fatal("GET /network/provider-status documents no client_id")
	}
	if clientId["required"] == true || asMap(clientId["schema"])["format"] != "uuid" {
		t.Fatalf("client_id = %v, want an optional uuid", clientId)
	}
	format, ok := parameters["format"]
	if !ok {
		t.Fatal("GET /network/provider-status documents no format")
	}
	formats := []string{}
	for _, value := range asSlice(asMap(format["schema"])["enum"]) {
		formats = append(formats, value.(string))
	}
	if format["required"] == true || !slices.Equal(formats, []string{controller.ProviderStatusFormatFull, controller.ProviderStatusFormatCompact}) {
		t.Fatalf("format = %v, want optional full or compact", format)
	}
	if len(parameters) != 2 {
		t.Fatalf("query parameters = %v", parameters)
	}

	// anyOf, not oneOf: a refused or empty answer has no statuses, so it is
	// both forms at once. The full form is listed last: TestSpecConformance
	// merges the members' properties in order, so the full form's are the
	// ones it checks against the route's registry type, as before the compact
	// form existed.
	_, response := spec.operation("GET", "/network/provider-status")
	refs := []string{}
	for _, member := range asSlice(response["anyOf"]) {
		refs = append(refs, asMap(member)["$ref"].(string))
	}
	if !slices.Equal(refs, []string{"#/components/schemas/GetProviderStatusCompactResult", "#/components/schemas/GetProviderStatusResult"}) {
		t.Fatalf("200 = %v, want any of the compact and the full result", response)
	}
}

// Both forms of the answer agree with the Go types field for field, in both
// directions: the spec promises nothing the server leaves out, and the server
// sends nothing the spec does not document.
func TestProviderStatusSpecMatchesBothForms(t *testing.T) {
	spec := loadSpec(t)
	checker := &conformanceChecker{t: t, spec: spec}
	for schemaName, goType := range map[string]reflect.Type{
		"GetProviderStatusResult":        rt(controller.GetProviderStatusResult{}),
		"GetProviderStatusCompactResult": rt(controller.GetProviderStatusCompactResult{}),
	} {
		schema := asMap(spec.schemas[schemaName])
		if schema == nil {
			t.Fatalf("the spec has no %s", schemaName)
		}
		checker.check(schemaName, "", schema, goType, 0)
		if missing := undocumentedJsonFields(spec, "", schema, goType); len(missing) != 0 {
			t.Fatalf("%s: the spec does not document %v", schemaName, missing)
		}
	}
}

// The spec lists the reasons a status gives, in the order the model applies
// them, in both forms.
func TestProviderStatusSpecReasons(t *testing.T) {
	spec := loadSpec(t)
	for _, schemaName := range []string{"ProviderStatus", "ProviderStatusCompact"} {
		reason := spec.resolve(asMap(spec.properties(asMap(spec.schemas[schemaName]))["reason"]))
		reasons := []string{}
		for _, value := range asSlice(reason["enum"]) {
			reasons = append(reasons, value.(string))
		}
		if !slices.Equal(reasons, model.ProviderStatusReasons) {
			t.Fatalf("%s.reason = %v, want %v", schemaName, reasons, model.ProviderStatusReasons)
		}
	}
}

// The spec lists the reason codes a status gives, in the order of the
// reasons they first code, in both forms, and documents each of them.
func TestProviderStatusSpecReasonCodes(t *testing.T) {
	spec := loadSpec(t)
	for _, schemaName := range []string{"ProviderStatus", "ProviderStatusCompact"} {
		schema := asMap(spec.schemas[schemaName])
		reasonCode := spec.resolve(asMap(spec.properties(schema)["reason_code"]))
		codes := []string{}
		for _, value := range asSlice(reasonCode["enum"]) {
			codes = append(codes, value.(string))
		}
		if !slices.Equal(codes, model.ProviderStatusCodes) {
			t.Fatalf("%s.reason_code = %v, want %v", schemaName, codes, model.ProviderStatusCodes)
		}
		if !slices.Contains(asSlice(schema["required"]), any("reason_code")) {
			t.Fatalf("%s does not require reason_code", schemaName)
		}
		description, _ := reasonCode["description"].(string)
		for _, code := range model.ProviderStatusCodes {
			if !strings.Contains(description, "`"+code+"`") {
				t.Fatalf("%s.reason_code does not document %s", schemaName, code)
			}
		}
	}
}

// The operation documents its conditional requests: an optional If-None-Match
// header, the ETag and Cache-Control of the 200, and a 304 with the ETag.
func TestProviderStatusSpecDocumentsConditionalRequests(t *testing.T) {
	spec := loadSpec(t)
	operation := asMap(asMap(spec.paths["/network/provider-status"])["get"])

	var ifNoneMatch map[string]any
	for _, parameterAny := range asSlice(operation["parameters"]) {
		parameter := asMap(parameterAny)
		if parameter["in"] == "header" && parameter["name"] == "If-None-Match" {
			ifNoneMatch = parameter
		}
	}
	if ifNoneMatch == nil || ifNoneMatch["required"] == true || asMap(ifNoneMatch["schema"])["type"] != "string" {
		t.Fatalf("If-None-Match = %v, want an optional string header", ifNoneMatch)
	}

	responses := asMap(operation["responses"])
	okHeaders := asMap(asMap(responses["200"])["headers"])
	if asMap(okHeaders["ETag"]) == nil || asMap(okHeaders["Cache-Control"]) == nil {
		t.Fatalf("200 headers = %v, want ETag and Cache-Control", okHeaders)
	}
	notModified := asMap(responses["304"])
	if notModified == nil || asMap(asMap(notModified["headers"])["ETag"]) == nil || notModified["content"] != nil {
		t.Fatalf("304 = %v, want the ETag and no content", notModified)
	}
}
