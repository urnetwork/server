package nginx

// Route file tests. The fixture, testdata/route_v1_example.json, is shared
// byte for byte with warp (warpctl/testdata/lbalt_route_v1_example.json),
// which renders it from a synthetic services.yml. Here it must parse, encode
// back to the same bytes, and turn into a refusal naming the field whenever
// one field of it is broken.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The shared fixture's bytes.
func routeFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "route_v1_example.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The fixture as a generic json tree, numbers kept as written, for a test to
// edit.
func routeFixtureTree(t *testing.T) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(routeFixture(t)))
	decoder.UseNumber()
	tree := map[string]any{}
	if err := decoder.Decode(&tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

// The value reached from tree through keys: object keys (string) and array
// indexes (int).
func routeTreeValue(t *testing.T, tree map[string]any, keys ...any) any {
	t.Helper()
	var value any = tree
	for _, key := range keys {
		switch key := key.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("%v: no object holds %q", keys, key)
			}
			value, ok = object[key]
			if !ok {
				t.Fatalf("%v: no field %q", keys, key)
			}
		case int:
			array, ok := value.([]any)
			if !ok || len(array) <= key {
				t.Fatalf("%v: no element %d", keys, key)
			}
			value = array[key]
		default:
			t.Fatalf("%v: %v is neither a key nor an index", keys, key)
		}
	}
	return value
}

// Replaces the existing value at keys.
func setRouteTreeValue(t *testing.T, tree map[string]any, value any, keys ...any) {
	t.Helper()
	parent := routeTreeValue(t, tree, keys[:len(keys)-1]...)
	switch key := keys[len(keys)-1].(type) {
	case string:
		object, ok := parent.(map[string]any)
		if !ok {
			t.Fatalf("%v: no object holds %q", keys, key)
		}
		if _, ok := object[key]; !ok {
			t.Fatalf("%v: no field %q", keys, key)
		}
		object[key] = value
	case int:
		array, ok := parent.([]any)
		if !ok || len(array) <= key {
			t.Fatalf("%v: no element %d", keys, key)
		}
		array[key] = value
	default:
		t.Fatalf("%v: %v is neither a key nor an index", keys, key)
	}
}

func encodeRouteTree(t *testing.T, tree map[string]any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The field refusal err carries, or nil.
func routeFieldErrorOf(err error) *routeFieldError {
	var fieldError *routeFieldError
	if errors.As(err, &fieldError) {
		return fieldError
	}
	return nil
}

// One object field of a generic tree: the keys that reach it, the last its
// own, and its path written as refusals write it.
type routeTreeField struct {
	keys      []any
	fieldPath string
}

// Every object field under value, depth first.
func listRouteTreeFields(value any, keys []any, fieldPath string) []*routeTreeField {
	routeTreeFields := []*routeTreeField{}
	switch value := value.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(value)) {
			childKeys := append(slices.Clone(keys), key)
			childPath := key
			if fieldPath != "" {
				childPath = fieldPath + "." + key
			}
			routeTreeFields = append(routeTreeFields, &routeTreeField{
				keys:      childKeys,
				fieldPath: childPath,
			})
			routeTreeFields = append(routeTreeFields, listRouteTreeFields(value[key], childKeys, childPath)...)
		}
	case []any:
		for i, element := range value {
			elementKeys := append(slices.Clone(keys), i)
			routeTreeFields = append(routeTreeFields, listRouteTreeFields(element, elementKeys, fmt.Sprintf("%s[%d]", fieldPath, i))...)
		}
	}
	return routeTreeFields
}

// The fixture parses, and its values land in their fields.
func TestParseRouteTableReadsTheFixture(t *testing.T) {
	routeTable, err := ParseRouteTable(routeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		fieldPath string
		got       any
		want      any
	}{
		{fieldPath: "schema_version", got: routeTable.SchemaVersion, want: RouteSchemaVersion},
		{fieldPath: "env", got: routeTable.Env, want: "test"},
		{fieldPath: "host", got: routeTable.Host, want: "edge-a.example"},
		{fieldPath: "generation", got: routeTable.Generation, want: "test-connect-g1-0123456789abcdef"},
		{fieldPath: "domains", got: routeTable.Domains, want: []string{"service.example", "other.example"}},
		{fieldPath: "upstream", got: *routeTable.Upstream, want: RouteUpstream{Host: "warpservices", Ipv4: "192.0.2.1"}},
		{fieldPath: "lb.names", got: routeTable.Lb.Names, want: []string{"test-lb.service.example", "test-lb.other.example"}},
		{fieldPath: "lb.route_prefixes", got: routeTable.Lb.RoutePrefixes, want: []string{"/hidden-example"}},
		{fieldPath: "no_access_log", got: routeTable.NoAccessLog, want: []string{"quiet.service.example"}},
		{fieldPath: "interfaces[0].lb_alt", got: routeTable.Interfaces[0].LbAlt, want: true},
		{fieldPath: "interfaces[0].ipv6", got: routeTable.Interfaces[0].Ipv6, want: "2001:db8:1::1"},
		{fieldPath: "interfaces[1].ipv6", got: routeTable.Interfaces[1].Ipv6, want: ""},
		{
			fieldPath: "interfaces[1].rate_limit",
			got:       *routeTable.Interfaces[1].RateLimit,
			want: RouteRateLimit{
				RequestsPerMinute: 450,
				Burst:             150,
				NetConnections:    50,
				ExcludeSubnets:    []string{"10.0.0.0/8", "172.16.0.0/12"},
			},
		},
		{
			fieldPath: "listen",
			got:       *routeTable.Listen,
			want:      RouteListen{HttpPort: 7080, HttpsPort: 7443, QuicPort: 7443, WhodisPort: 7453},
		},
		{
			fieldPath: "front",
			got:       *routeTable.Front,
			want:      RouteFront{ConnectService: "connect", ApiService: "api", WhodisTlds: []string{"whodis.example."}},
		},
		{fieldPath: "services[0].redirect_aliases[0]", got: *routeTable.Services[0].RedirectAliases[0], want: RouteRedirectAlias{Name: "old-api.service.example", Target: "api.service.example"}},
		{fieldPath: "services[0].route_rate_limits[0]", got: *routeTable.Services[0].RouteRateLimits[0], want: RouteRateLimitRule{Path: "/connect/control", RequestsPerMinute: 12000, Burst: 6000}},
		{
			fieldPath: "services[0].route_request_bodies[0]",
			got:       *routeTable.Services[0].RouteRequestBodies[0],
			want:      RouteRequestBody{Path: "/upload/artifact", ClientBodyBufferSize: 64 * 1024, ClientBodyTimeoutSeconds: 60},
		},
		{fieldPath: "services[0].no_success_access_log", got: routeTable.Services[0].NoSuccessAccessLog, want: []string{"/connect/control", "/hello"}},
		{
			fieldPath: "services[0].keepalive",
			got:       *routeTable.Services[0].Keepalive,
			want:      RouteKeepalive{Connections: 1024, Requests: 8192, TimeSeconds: 900, TimeoutSeconds: 60},
		},
		{fieldPath: "services[1].names[0]", got: routeTable.Services[1].Names[0], want: "*.connect.service.example"},
		{fieldPath: "services[1].websocket", got: routeTable.Services[1].Websocket, want: true},
		{fieldPath: "services[1].streamable_paths", got: routeTable.Services[1].StreamablePaths, want: []string{}},
		{fieldPath: "services[1].blocks[1]", got: *routeTable.Services[1].Blocks[1], want: RouteBlock{Name: "g1", Weight: 3, Port: 7202}},
		{fieldPath: "services[2].streamable", got: routeTable.Services[2].Streamable, want: true},
		{fieldPath: "services[2].stateful", got: routeTable.Services[2].Stateful, want: true},
		{fieldPath: "services[2].route_prefixes", got: routeTable.Services[2].RoutePrefixes, want: []string{""}},
	}
	for _, check := range checks {
		if !reflect.DeepEqual(check.got, check.want) {
			t.Errorf("%s = %#v, want %#v", check.fieldPath, check.got, check.want)
		}
	}
}

// The structs declare the file's fields in its order under its keys, so a
// parsed fixture encoded as warpctl encodes it is the fixture, byte for byte.
func TestRouteTableEncodingReproducesTheFixture(t *testing.T) {
	raw := routeFixture(t)
	routeTable, err := ParseRouteTable(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(routeTable, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if bytes.Equal(encoded, raw) {
		return
	}
	encodedLines := strings.Split(string(encoded), "\n")
	rawLines := strings.Split(string(raw), "\n")
	for i := range max(len(encodedLines), len(rawLines)) {
		encodedLine := ""
		if i < len(encodedLines) {
			encodedLine = encodedLines[i]
		}
		rawLine := ""
		if i < len(rawLines) {
			rawLine = rawLines[i]
		}
		if encodedLine != rawLine {
			t.Fatalf("line %d: encoded %q, fixture %q", i+1, encodedLine, rawLine)
		}
	}
	t.Fatalf("encoded table differs from the fixture")
}

// The revision is the lowercase hex sha256 of the file's exact bytes, so the
// same table in other bytes is another revision.
func TestLoadRouteTableRevisionIsTheFileSha256(t *testing.T) {
	raw := routeFixture(t)
	dir := t.TempDir()
	routeFilePath := filepath.Join(dir, "route.json")
	if err := os.WriteFile(routeFilePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	routeTable, revision, err := LoadRouteTable(routeFilePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if want := hex.EncodeToString(digest[:]); revision != want {
		t.Fatalf("revision = %s, want %s", revision, want)
	}
	if strings.ToLower(revision) != revision || len(revision) != 2*sha256.Size {
		t.Fatalf("revision %s is not lowercase hex", revision)
	}
	parsedTable, err := ParseRouteTable(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(routeTable, parsedTable) {
		t.Fatalf("loaded table differs from the parsed fixture")
	}

	paddedRaw := append(slices.Clone(raw), '\n')
	paddedRouteFilePath := filepath.Join(dir, "route-padded.json")
	if err := os.WriteFile(paddedRouteFilePath, paddedRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	paddedRouteTable, paddedRevision, err := LoadRouteTable(paddedRouteFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paddedRouteTable, parsedTable) {
		t.Fatalf("padded file parsed to another table")
	}
	paddedDigest := sha256.Sum256(paddedRaw)
	if want := hex.EncodeToString(paddedDigest[:]); paddedRevision != want || paddedRevision == revision {
		t.Fatalf("padded revision = %s, want %s, unlike %s", paddedRevision, want, revision)
	}
}

// A file that can't be read or parsed yields no table and no revision.
func TestLoadRouteTableRefusesAMissingOrInvalidFile(t *testing.T) {
	dir := t.TempDir()
	tree := routeFixtureTree(t)
	tree["schema_version"] = 2
	invalidRouteFilePath := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalidRouteFilePath, encodeRouteTree(t, tree), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, routeFilePath := range []string{filepath.Join(dir, "missing.json"), invalidRouteFilePath} {
		routeTable, revision, err := LoadRouteTable(routeFilePath)
		if err == nil || routeTable != nil || revision != "" {
			t.Errorf("%s: table %v, revision %q, err %v; want only an error", routeFilePath, routeTable, revision, err)
		}
	}
}

// A key outside the schema is refused at every level, including a key
// encoding/json would match to a field in another case.
func TestParseRouteTableRefusesUnknownFields(t *testing.T) {
	cases := []struct {
		mutate    func(t *testing.T, tree map[string]any)
		fieldPath string
	}{
		{
			mutate: func(t *testing.T, tree map[string]any) {
				tree["extra"] = json.Number("1")
			},
			fieldPath: "extra",
		},
		{
			mutate: func(t *testing.T, tree map[string]any) {
				routeTreeValue(t, tree, "services", 0, "blocks", 1).(map[string]any)["extra"] = "x"
			},
			fieldPath: "services[0].blocks[1].extra",
		},
		{
			mutate: func(t *testing.T, tree map[string]any) {
				routeTreeValue(t, tree, "interfaces", 1, "rate_limit").(map[string]any)["delay"] = json.Number("0")
			},
			fieldPath: "interfaces[1].rate_limit.delay",
		},
		{
			// beside the field it would shadow
			mutate: func(t *testing.T, tree map[string]any) {
				tree["ENV"] = "other"
			},
			fieldPath: "ENV",
		},
		{
			// in place of the field
			mutate: func(t *testing.T, tree map[string]any) {
				listen := routeTreeValue(t, tree, "listen").(map[string]any)
				listen["Http_Port"] = listen["http_port"]
				delete(listen, "http_port")
			},
			fieldPath: "listen.Http_Port",
		},
	}
	for _, c := range cases {
		tree := routeFixtureTree(t)
		c.mutate(t, tree)
		_, err := ParseRouteTable(encodeRouteTree(t, tree))
		fieldError := routeFieldErrorOf(err)
		if fieldError == nil || fieldError.fieldPath != c.fieldPath || !strings.Contains(fieldError.message, "not a field") {
			t.Errorf("%s: err = %v, want it refused as not a field", c.fieldPath, err)
		}
	}
}

// Only whitespace may follow the table.
func TestParseRouteTableRefusesTrailingData(t *testing.T) {
	raw := routeFixture(t)
	for _, trailing := range []string{"{}", "x", "[]", "1", "\"route\"", "}"} {
		_, err := ParseRouteTable(append(slices.Clone(raw), trailing...))
		if err == nil || !strings.Contains(err.Error(), "trailing data") {
			t.Errorf("trailing %q: err = %v, want trailing data refused", trailing, err)
		}
	}
	if _, err := ParseRouteTable(append(slices.Clone(raw), " \t\r\n\n"...)); err != nil {
		t.Fatalf("trailing whitespace: %v", err)
	}
}

// A schema version other than 1 is refused, before any field it may have
// renamed.
func TestParseRouteTableRefusesSchemaVersion2(t *testing.T) {
	cases := []struct {
		mutate func(tree map[string]any)
	}{
		{mutate: func(tree map[string]any) { tree["schema_version"] = 2 }},
		{mutate: func(tree map[string]any) { tree["schema_version"] = 0 }},
		{mutate: func(tree map[string]any) { tree["schema_version"] = "1" }},
		{mutate: func(tree map[string]any) { tree["schema_version"] = json.Number("1.0") }},
		{
			mutate: func(tree map[string]any) {
				tree["schema_version"] = 2
				tree["environment"] = tree["env"]
				delete(tree, "env")
			},
		},
	}
	for i, c := range cases {
		tree := routeFixtureTree(t)
		c.mutate(tree)
		_, err := ParseRouteTable(encodeRouteTree(t, tree))
		fieldError := routeFieldErrorOf(err)
		if fieldError == nil || fieldError.fieldPath != "schema_version" {
			t.Errorf("case %d: err = %v, want schema_version refused", i, err)
		}
	}
}

// warpctl writes every field, so a missing one means another writer or
// schema: each is refused, at every depth.
func TestParseRouteTableRefusesEveryMissingField(t *testing.T) {
	routeTreeFields := listRouteTreeFields(routeFixtureTree(t), []any{}, "")
	fieldPaths := []string{}
	for _, field := range routeTreeFields {
		fieldPaths = append(fieldPaths, field.fieldPath)
	}
	for _, fieldPath := range []string{"schema_version", "interfaces[1].rate_limit.exclude_subnets", "services[2].blocks[0].port", "services[0].redirect_aliases[0].target"} {
		if !slices.Contains(fieldPaths, fieldPath) {
			t.Fatalf("the fixture's fields miss %s", fieldPath)
		}
	}
	for _, field := range routeTreeFields {
		tree := routeFixtureTree(t)
		parent := routeTreeValue(t, tree, field.keys[:len(field.keys)-1]...).(map[string]any)
		delete(parent, field.keys[len(field.keys)-1].(string))
		_, err := ParseRouteTable(encodeRouteTree(t, tree))
		fieldError := routeFieldErrorOf(err)
		if fieldError == nil || fieldError.fieldPath != field.fieldPath || fieldError.message != "is missing" {
			t.Errorf("without %s: err = %v, want it missing", field.fieldPath, err)
		}
	}
}

// warpctl never writes null, not even for an empty array, so a null field is
// refused, at every depth.
func TestParseRouteTableRefusesEveryNullField(t *testing.T) {
	for _, field := range listRouteTreeFields(routeFixtureTree(t), []any{}, "") {
		tree := routeFixtureTree(t)
		setRouteTreeValue(t, tree, nil, field.keys...)
		_, err := ParseRouteTable(encodeRouteTree(t, tree))
		fieldError := routeFieldErrorOf(err)
		if fieldError == nil || fieldError.fieldPath != field.fieldPath || fieldError.message != "must not be null" {
			t.Errorf("null %s: err = %v, want null refused", field.fieldPath, err)
		}
	}
}

// Objects, arrays and their elements must have the schema's shape, and the
// root must be an object.
func TestParseRouteTableRefusesWrongShapes(t *testing.T) {
	cases := []struct {
		keys      []any
		value     any
		fieldPath string
		message   string
	}{
		{keys: []any{"upstream"}, value: []any{}, fieldPath: "upstream", message: "must be an object"},
		{keys: []any{"interfaces", 0, "rate_limit"}, value: "450r/m", fieldPath: "interfaces[0].rate_limit", message: "must be an object"},
		{keys: []any{"domains"}, value: "service.example", fieldPath: "domains", message: "must be an array"},
		{keys: []any{"services", 0, "blocks"}, value: map[string]any{}, fieldPath: "services[0].blocks", message: "must be an array"},
		{keys: []any{"services", 0}, value: json.Number("1"), fieldPath: "services[0]", message: "must be an object"},
		{keys: []any{"services", 1}, value: nil, fieldPath: "services[1]", message: "must not be null"},
		{keys: []any{"domains", 0}, value: nil, fieldPath: "domains[0]", message: "must not be null"},
	}
	for _, c := range cases {
		tree := routeFixtureTree(t)
		setRouteTreeValue(t, tree, c.value, c.keys...)
		_, err := ParseRouteTable(encodeRouteTree(t, tree))
		fieldError := routeFieldErrorOf(err)
		if fieldError == nil || fieldError.fieldPath != c.fieldPath || fieldError.message != c.message {
			t.Errorf("%s: err = %v, want %q", c.fieldPath, err, c.message)
		}
	}

	for _, raw := range []string{"[]", "null", "\"route\"", "1"} {
		_, err := ParseRouteTable([]byte(raw))
		fieldError := routeFieldErrorOf(err)
		if fieldError == nil || fieldError.fieldPath != "" {
			t.Errorf("root %s: err = %v, want the root refused", raw, err)
		}
	}
	for _, raw := range []string{"", " \n", "{", "{\"schema_version\": 1,}"} {
		if _, err := ParseRouteTable([]byte(raw)); err == nil {
			t.Errorf("%q parsed", raw)
		}
	}

	// a scalar of another type is the typed decode's refusal, which names the
	// field too
	tree := routeFixtureTree(t)
	setRouteTreeValue(t, tree, json.Number("5"), "env")
	if _, err := ParseRouteTable(encodeRouteTree(t, tree)); err == nil || !strings.Contains(err.Error(), "env") {
		t.Errorf("numeric env: err = %v, want a refusal naming env", err)
	}
}

// Each validation rule refuses its field, named by its path. One case per
// rule and field; each breaks one value of the fixture.
func TestParseRouteTableValidation(t *testing.T) {
	longLabel := strings.Repeat("a", maxRouteLabelBytes+1)
	longName := strings.Repeat("a.", maxRouteNameBytes/2) + "example"
	rateLimitRule := map[string]any{"path": "/connect/control", "requests_per_minute": 12000, "burst": 6000}
	requestBody := map[string]any{"path": "/upload/artifact", "client_body_buffer_size": 65536, "client_body_timeout_seconds": 60}
	cases := []struct {
		keys      []any
		value     any
		fieldPath string
		message   string
	}{
		// identities
		{keys: []any{"env"}, value: "", fieldPath: "env", message: "must not be empty"},
		{keys: []any{"host"}, value: "", fieldPath: "host", message: "must not be empty"},
		{keys: []any{"generation"}, value: "", fieldPath: "generation", message: "must not be empty"},
		{keys: []any{"generation"}, value: "test connect/g1", fieldPath: "generation", message: "may hold only"},

		// domains
		{keys: []any{"domains"}, value: []any{}, fieldPath: "domains", message: "at least"},
		{keys: []any{"domains", 1}, value: "Other.example", fieldPath: "domains[1]", message: "lowercase"},
		{keys: []any{"domains", 1}, value: "*.other.example", fieldPath: "domains[1]", message: "exact name"},
		{keys: []any{"domains", 0}, value: "service..example", fieldPath: "domains[0]", message: "empty label"},
		{keys: []any{"domains", 0}, value: "service.example.", fieldPath: "domains[0]", message: "empty label"},
		{keys: []any{"domains", 0}, value: longLabel + ".example", fieldPath: "domains[0]", message: "label over"},
		{keys: []any{"domains", 0}, value: longName, fieldPath: "domains[0]", message: "at most"},
		{keys: []any{"domains"}, value: []any{"service.example", "service.example"}, fieldPath: "domains[1]", message: "duplicates domains[0]"},

		// upstream
		{keys: []any{"upstream", "host"}, value: "", fieldPath: "upstream.host", message: "must not be empty"},
		{keys: []any{"upstream", "ipv4"}, value: "", fieldPath: "upstream.ipv4", message: "ip address"},
		{keys: []any{"upstream", "ipv4"}, value: "2001:db8::1", fieldPath: "upstream.ipv4", message: "ipv4"},

		// lb
		{keys: []any{"lb", "names"}, value: []any{}, fieldPath: "lb.names", message: "at least"},
		{keys: []any{"lb", "names", 0}, value: "*.service.example", fieldPath: "lb.names[0]", message: "exact name"},
		{keys: []any{"lb", "names", 1}, value: "Test-lb.other.example", fieldPath: "lb.names[1]", message: "lowercase"},
		{keys: []any{"lb", "names", 1}, value: "test-lb.service.example", fieldPath: "lb.names[1]", message: "duplicates lb.names[0]"},
		{keys: []any{"lb", "route_prefixes"}, value: []any{}, fieldPath: "lb.route_prefixes", message: "at least"},
		{keys: []any{"lb", "route_prefixes", 0}, value: "hidden-example", fieldPath: "lb.route_prefixes[0]", message: "start with /"},
		{keys: []any{"lb", "route_prefixes"}, value: []any{"/hidden-example", "/hidden-example"}, fieldPath: "lb.route_prefixes[1]", message: "duplicates lb.route_prefixes[0]"},

		// no_access_log
		{keys: []any{"no_access_log", 0}, value: "*.service.example", fieldPath: "no_access_log[0]", message: "exact name"},
		{keys: []any{"no_access_log"}, value: []any{"quiet.service.example", "quiet.service.example"}, fieldPath: "no_access_log[1]", message: "duplicates no_access_log[0]"},

		// interfaces
		{keys: []any{"interfaces"}, value: []any{}, fieldPath: "interfaces", message: "at least"},
		{keys: []any{"interfaces", 0, "name"}, value: "", fieldPath: "interfaces[0].name", message: "must not be empty"},
		{keys: []any{"interfaces", 1, "name"}, value: "eth1", fieldPath: "interfaces[1].name", message: "duplicates interfaces[0].name"},
		{keys: []any{"interfaces", 0, "block"}, value: "", fieldPath: "interfaces[0].block", message: "must not be empty"},
		{keys: []any{"interfaces", 1, "block"}, value: "edge-a.example-eth1", fieldPath: "interfaces[1].block", message: "duplicates interfaces[0].block"},
		{keys: []any{"interfaces", 0, "ipv4"}, value: "198.51.100.300", fieldPath: "interfaces[0].ipv4", message: "ip address"},
		{keys: []any{"interfaces", 0, "ipv4"}, value: "2001:db8:1::2", fieldPath: "interfaces[0].ipv4", message: "ipv4"},
		{keys: []any{"interfaces", 0, "ipv6"}, value: "198.51.100.3", fieldPath: "interfaces[0].ipv6", message: "ipv6"},
		{keys: []any{"interfaces", 0, "ipv6"}, value: "::ffff:198.51.100.3", fieldPath: "interfaces[0].ipv6", message: "ipv6"},
		{keys: []any{"interfaces", 0, "ipv6"}, value: "2001:db8:1::1%eth1", fieldPath: "interfaces[0].ipv6", message: "zone"},
		{keys: []any{"interfaces", 0, "ipv6"}, value: "2001:DB8:1::1", fieldPath: "interfaces[0].ipv6", message: "written 2001:db8:1::1"},
		{keys: []any{"interfaces", 1, "ipv4"}, value: "", fieldPath: "interfaces[1]", message: "needs an ipv4 or ipv6 address"},
		{keys: []any{"interfaces", 1, "ipv4"}, value: "198.51.100.1", fieldPath: "interfaces[1].ipv4", message: "duplicates interfaces[0].ipv4"},
		{keys: []any{"interfaces", 0, "ipv4"}, value: "192.0.2.1", fieldPath: "interfaces[0].ipv4", message: "services bridge"},
		{keys: []any{"interfaces", 0, "rate_limit", "requests_per_minute"}, value: -1, fieldPath: "interfaces[0].rate_limit.requests_per_minute", message: "negative"},
		{keys: []any{"interfaces", 0, "rate_limit", "burst"}, value: -1, fieldPath: "interfaces[0].rate_limit.burst", message: "negative"},
		{keys: []any{"interfaces", 0, "rate_limit", "net_connections"}, value: -1, fieldPath: "interfaces[0].rate_limit.net_connections", message: "negative"},
		{keys: []any{"interfaces", 0, "rate_limit", "exclude_subnets", 0}, value: "203.0.113.0/33", fieldPath: "interfaces[0].rate_limit.exclude_subnets[0]", message: "cidr"},
		{keys: []any{"interfaces", 0, "rate_limit", "exclude_subnets"}, value: []any{"203.0.113.0/24", "203.0.113.0/24"}, fieldPath: "interfaces[0].rate_limit.exclude_subnets[1]", message: "duplicates interfaces[0].rate_limit.exclude_subnets[0]"},

		// listen
		{keys: []any{"listen", "http_port"}, value: 0, fieldPath: "listen.http_port", message: "1-65535"},
		{keys: []any{"listen", "https_port"}, value: 65536, fieldPath: "listen.https_port", message: "1-65535"},
		{keys: []any{"listen", "quic_port"}, value: 0, fieldPath: "listen.quic_port", message: "1-65535"},
		{keys: []any{"listen", "whodis_port"}, value: -1, fieldPath: "listen.whodis_port", message: "1-65535"},
		{keys: []any{"listen", "https_port"}, value: 7080, fieldPath: "listen.https_port", message: "listen.http_port"},
		{keys: []any{"listen", "whodis_port"}, value: 7443, fieldPath: "listen.whodis_port", message: "listen.quic_port"},

		// services
		{keys: []any{"services", 0, "name"}, value: "", fieldPath: "services[0].name", message: "must not be empty"},
		{keys: []any{"services", 2, "name"}, value: "api", fieldPath: "services[2].name", message: "duplicates services[0].name"},
		{keys: []any{"services", 0, "status"}, value: "", fieldPath: "services[0].status", message: "must not be empty"},
		{keys: []any{"services", 0, "names", 0}, value: "Api.service.example", fieldPath: "services[0].names[0]", message: "lowercase"},
		{keys: []any{"services", 1, "names", 0}, value: "*.*.connect.service.example", fieldPath: "services[1].names[0]", message: "lowercase"},
		{keys: []any{"services", 1, "names", 0}, value: "*.", fieldPath: "services[1].names[0]", message: "must name a host"},
		{keys: []any{"services", 2, "names", 0}, value: "api.service.example", fieldPath: "services[2].names[0]", message: "duplicates services[0].names[0]"},
		{keys: []any{"services", 0, "names", 0}, value: "test-lb.service.example", fieldPath: "services[0].names[0]", message: "duplicates lb.names[0]"},
		{keys: []any{"services", 0, "redirect_aliases", 0, "name"}, value: "Old-api.service.example", fieldPath: "services[0].redirect_aliases[0].name", message: "lowercase"},
		{keys: []any{"services", 0, "redirect_aliases", 0, "name"}, value: "api.service.example", fieldPath: "services[0].redirect_aliases[0].name", message: "duplicates services[0].names[0]"},
		{keys: []any{"services", 0, "redirect_aliases", 0, "target"}, value: "*.service.example", fieldPath: "services[0].redirect_aliases[0].target", message: "exact name"},
		{keys: []any{"services", 0, "cors_origins", 0}, value: "", fieldPath: "services[0].cors_origins[0]", message: "must not be empty"},
		{keys: []any{"services", 0, "cors_origins", 0}, value: "https://app.service.example extra", fieldPath: "services[0].cors_origins[0]", message: "whitespace"},
		{keys: []any{"services", 0, "cors_origins"}, value: []any{"*", "*"}, fieldPath: "services[0].cors_origins[1]", message: "duplicates services[0].cors_origins[0]"},
		{keys: []any{"services", 2, "streamable_paths"}, value: []any{"stream"}, fieldPath: "services[2].streamable_paths[0]", message: "start with /"},
		{keys: []any{"services", 2, "streamable_paths"}, value: []any{"/stream\u0001"}, fieldPath: "services[2].streamable_paths[0]", message: "control"},
		{keys: []any{"services", 2, "streamable_paths"}, value: []any{"/stream("}, fieldPath: "services[2].streamable_paths[0]", message: "regular expression"},
		{keys: []any{"services", 2, "streamable_paths"}, value: []any{"/stream", "/stream"}, fieldPath: "services[2].streamable_paths[1]", message: "duplicates services[2].streamable_paths[0]"},
		{keys: []any{"services", 0, "route_prefixes"}, value: []any{}, fieldPath: "services[0].route_prefixes", message: "at least"},
		{keys: []any{"services", 0, "route_prefixes", 0}, value: "hidden", fieldPath: "services[0].route_prefixes[0]", message: "start with /"},
		{keys: []any{"services", 0, "route_prefixes"}, value: []any{"", ""}, fieldPath: "services[0].route_prefixes[1]", message: "duplicates services[0].route_prefixes[0]"},
		{keys: []any{"services", 0, "route_rate_limits", 0, "path"}, value: "connect/control", fieldPath: "services[0].route_rate_limits[0].path", message: "start with /"},
		{keys: []any{"services", 0, "route_rate_limits", 0, "path"}, value: "/connect control", fieldPath: "services[0].route_rate_limits[0].path", message: "whitespace"},
		{keys: []any{"services", 0, "route_rate_limits", 0, "requests_per_minute"}, value: 0, fieldPath: "services[0].route_rate_limits[0].requests_per_minute", message: "positive"},
		{keys: []any{"services", 0, "route_rate_limits", 0, "burst"}, value: -1, fieldPath: "services[0].route_rate_limits[0].burst", message: "negative"},
		{keys: []any{"services", 0, "route_rate_limits"}, value: []any{rateLimitRule, rateLimitRule}, fieldPath: "services[0].route_rate_limits[1].path", message: "duplicates services[0].route_rate_limits[0].path"},
		{keys: []any{"services", 0, "route_request_bodies", 0, "path"}, value: "upload/artifact", fieldPath: "services[0].route_request_bodies[0].path", message: "start with /"},
		{keys: []any{"services", 0, "route_request_bodies", 0, "client_body_buffer_size"}, value: 0, fieldPath: "services[0].route_request_bodies[0].client_body_buffer_size", message: "positive"},
		{keys: []any{"services", 0, "route_request_bodies", 0, "client_body_timeout_seconds"}, value: 0, fieldPath: "services[0].route_request_bodies[0].client_body_timeout_seconds", message: "positive"},
		{keys: []any{"services", 0, "route_request_bodies"}, value: []any{requestBody, requestBody}, fieldPath: "services[0].route_request_bodies[1].path", message: "duplicates services[0].route_request_bodies[0].path"},
		{keys: []any{"services", 0, "no_success_access_log", 0}, value: "connect/control", fieldPath: "services[0].no_success_access_log[0]", message: "start with /"},
		{keys: []any{"services", 0, "no_success_access_log", 1}, value: "/connect/control", fieldPath: "services[0].no_success_access_log[1]", message: "duplicates services[0].no_success_access_log[0]"},
		{keys: []any{"services", 0, "keepalive", "connections"}, value: -1, fieldPath: "services[0].keepalive.connections", message: "negative"},
		{keys: []any{"services", 0, "keepalive", "requests"}, value: -1, fieldPath: "services[0].keepalive.requests", message: "negative"},
		{keys: []any{"services", 0, "keepalive", "time_seconds"}, value: -1, fieldPath: "services[0].keepalive.time_seconds", message: "negative"},
		{keys: []any{"services", 0, "keepalive", "timeout_seconds"}, value: -1, fieldPath: "services[0].keepalive.timeout_seconds", message: "negative"},
		{keys: []any{"services", 0, "blocks", 0, "name"}, value: "", fieldPath: "services[0].blocks[0].name", message: "must not be empty"},
		{keys: []any{"services", 0, "blocks", 1, "name"}, value: "g1", fieldPath: "services[0].blocks[1].name", message: "duplicates services[0].blocks[0].name"},
		{keys: []any{"services", 0, "blocks", 0, "weight"}, value: -1, fieldPath: "services[0].blocks[0].weight", message: "negative"},
		{keys: []any{"services", 0, "blocks", 0, "port"}, value: 0, fieldPath: "services[0].blocks[0].port", message: "1-65535"},
		{keys: []any{"services", 0, "blocks", 0, "port"}, value: 65536, fieldPath: "services[0].blocks[0].port", message: "1-65535"},
		{keys: []any{"services", 1, "blocks", 0, "port"}, value: 7101, fieldPath: "services[1].blocks[0].port", message: "duplicates services[0].blocks[0].port"},

		// front
		{keys: []any{"front", "connect_service"}, value: "missing", fieldPath: "front.connect_service", message: "must name a service"},
		{keys: []any{"front", "api_service"}, value: "missing", fieldPath: "front.api_service", message: "must name a service"},
		{keys: []any{"front", "api_service"}, value: "connect", fieldPath: "front.api_service", message: "must differ"},
		{keys: []any{"front", "whodis_tlds"}, value: []any{}, fieldPath: "front.whodis_tlds", message: "at least"},
		{keys: []any{"front", "whodis_tlds", 0}, value: "whodis.example", fieldPath: "front.whodis_tlds[0]", message: "end with ."},
		{keys: []any{"front", "whodis_tlds", 0}, value: "Whodis.example.", fieldPath: "front.whodis_tlds[0]", message: "lowercase"},
		{keys: []any{"front", "whodis_tlds"}, value: []any{"whodis.example.", "whodis.example."}, fieldPath: "front.whodis_tlds[1]", message: "duplicates front.whodis_tlds[0]"},
	}
	for _, c := range cases {
		tree := routeFixtureTree(t)
		setRouteTreeValue(t, tree, c.value, c.keys...)
		routeTable, err := ParseRouteTable(encodeRouteTree(t, tree))
		fieldError := routeFieldErrorOf(err)
		if fieldError == nil || fieldError.fieldPath != c.fieldPath || !strings.Contains(fieldError.message, c.message) {
			t.Errorf("%v = %#v: table %v, err = %v; want %s refused with %q", c.keys, c.value, routeTable != nil, err, c.fieldPath, c.message)
		}
	}
}

// The bounds the contract allows parse: zero weights, rates and caps, a
// fractional rate, an IPv6-only interface, the extreme ports, a service with
// no names, and hidden service prefixes.
func TestParseRouteTableAcceptsBoundaryValues(t *testing.T) {
	tree := routeFixtureTree(t)
	sets := []struct {
		keys  []any
		value any
	}{
		{keys: []any{"no_access_log"}, value: []any{}},
		{keys: []any{"lb", "route_prefixes"}, value: []any{""}},
		{keys: []any{"interfaces", 0, "rate_limit", "requests_per_minute"}, value: 0},
		{keys: []any{"interfaces", 0, "rate_limit", "burst"}, value: 0},
		{keys: []any{"interfaces", 0, "rate_limit", "net_connections"}, value: 0},
		{keys: []any{"interfaces", 0, "rate_limit", "exclude_subnets"}, value: []any{"2001:db8::/32"}},
		{keys: []any{"interfaces", 1, "rate_limit", "requests_per_minute"}, value: json.Number("7.5")},
		{keys: []any{"interfaces", 1, "ipv4"}, value: ""},
		{keys: []any{"interfaces", 1, "ipv6"}, value: "2001:db8:2::1"},
		{keys: []any{"listen", "http_port"}, value: 1},
		{keys: []any{"listen", "https_port"}, value: 65535},
		{keys: []any{"services", 0, "route_prefixes"}, value: []any{"/hidden-example", "/other-hidden-example"}},
		{keys: []any{"services", 0, "route_rate_limits", 0, "requests_per_minute"}, value: json.Number("0.5")},
		{keys: []any{"services", 0, "keepalive"}, value: map[string]any{"connections": 0, "requests": 0, "time_seconds": 0, "timeout_seconds": 0}},
		{keys: []any{"services", 0, "blocks", 0, "weight"}, value: 0},
		{keys: []any{"services", 0, "cors_origins"}, value: []any{"*"}},
		{keys: []any{"services", 1, "names", 0}, value: "*.a-b_c.connect.service.example"},
		{keys: []any{"services", 2, "names"}, value: []any{}},
		{keys: []any{"services", 2, "streamable_paths"}, value: []any{"/stream/[a-z0-9]+", "/events"}},
		{keys: []any{"front", "whodis_tlds"}, value: []any{"whodis.example.", "x."}},
	}
	for _, set := range sets {
		setRouteTreeValue(t, tree, set.value, set.keys...)
	}
	routeTable, err := ParseRouteTable(encodeRouteTree(t, tree))
	if err != nil {
		t.Fatal(err)
	}
	if got := routeTable.Interfaces[1].RateLimit.RequestsPerMinute; got != 7.5 {
		t.Errorf("interfaces[1].rate_limit.requests_per_minute = %v, want 7.5", got)
	}
	if got := routeTable.Services[0].RouteRateLimits[0].RequestsPerMinute; got != 0.5 {
		t.Errorf("services[0].route_rate_limits[0].requests_per_minute = %v, want 0.5", got)
	}
	if got := routeTable.Services[0].Blocks[0].Weight; got != 0 {
		t.Errorf("services[0].blocks[0].weight = %d, want 0", got)
	}
	if got := routeTable.Interfaces[1]; got.Ipv4 != "" || got.Ipv6 != "2001:db8:2::1" {
		t.Errorf("interfaces[1] = %s/%s, want IPv6 only", got.Ipv4, got.Ipv6)
	}
}

// Route prefixes are hidden, so a refusal of one never carries its value.
func TestParseRouteTableRefusalsOmitRoutePrefixes(t *testing.T) {
	secretRoutePrefix := "secret-prefix-example"
	cases := []struct {
		keys  []any
		value any
	}{
		{keys: []any{"lb", "route_prefixes"}, value: []any{secretRoutePrefix}},
		{keys: []any{"lb", "route_prefixes"}, value: []any{"/" + secretRoutePrefix, "/" + secretRoutePrefix}},
		{keys: []any{"services", 0, "route_prefixes"}, value: []any{secretRoutePrefix}},
		{keys: []any{"services", 0, "route_prefixes"}, value: []any{"/" + secretRoutePrefix, "/" + secretRoutePrefix}},
	}
	for _, c := range cases {
		tree := routeFixtureTree(t)
		setRouteTreeValue(t, tree, c.value, c.keys...)
		_, err := ParseRouteTable(encodeRouteTree(t, tree))
		if err == nil {
			t.Errorf("%v = %v parsed", c.keys, c.value)
			continue
		}
		if strings.Contains(err.Error(), secretRoutePrefix) {
			t.Errorf("%v: the refusal %q carries the prefix", c.keys, err)
		}
	}
}

// Lookups return the table's own entries, and the front's services resolve.
func TestRouteTableLookups(t *testing.T) {
	routeTable, err := ParseRouteTable(routeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if service, ok := routeTable.Service("mcp"); !ok || service != routeTable.Services[2] {
		t.Errorf("Service(mcp) = %v, %v; want services[2]", service, ok)
	}
	if service, ok := routeTable.Service("missing"); ok || service != nil {
		t.Errorf("Service(missing) = %v, %v; want none", service, ok)
	}
	if routeInterface, ok := routeTable.Interface("eth2"); !ok || routeInterface != routeTable.Interfaces[1] {
		t.Errorf("Interface(eth2) = %v, %v; want interfaces[1]", routeInterface, ok)
	}
	if routeInterface, ok := routeTable.Interface("eth9"); ok || routeInterface != nil {
		t.Errorf("Interface(eth9) = %v, %v; want none", routeInterface, ok)
	}
	if service := routeTable.ConnectService(); service != routeTable.Services[1] {
		t.Errorf("ConnectService() = %v, want services[1]", service)
	}
	if service := routeTable.ApiService(); service != routeTable.Services[0] {
		t.Errorf("ApiService() = %v, want services[0]", service)
	}

	// a table that names a missing service, which parsing refuses, has none
	unparsedRouteTable := &RouteTable{
		Front: &RouteFront{ConnectService: "connect", ApiService: "api"},
	}
	if service := unparsedRouteTable.ConnectService(); service != nil {
		t.Errorf("ConnectService() = %v, want nil", service)
	}
	if service := unparsedRouteTable.ApiService(); service != nil {
		t.Errorf("ApiService() = %v, want nil", service)
	}
}

// The lb names come first in their own order, the default server first, then
// the services' names and redirect aliases sorted.
func TestRouteTableServedNames(t *testing.T) {
	routeTable, err := ParseRouteTable(routeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"test-lb.service.example",
		"test-lb.other.example",
		"*.connect.service.example",
		"api.service.example",
		"connect.service.example",
		"mcp.service.example",
		"old-api.service.example",
		"test-api.other.example",
		"test-api.service.example",
		"test-connect.other.example",
		"test-connect.service.example",
		"test-mcp.other.example",
		"test-mcp.service.example",
	}
	servedNames := routeTable.ServedNames()
	if !slices.Equal(servedNames, want) {
		t.Fatalf("ServedNames() = %v, want %v", servedNames, want)
	}
	servedNames[0] = "changed.example"
	if routeTable.ServedNames()[0] != want[0] {
		t.Fatalf("ServedNames() shares its slice between calls")
	}
}

// A name listed twice is served once, at its first place.
func TestRouteTableServedNamesDeduplicates(t *testing.T) {
	routeTable := &RouteTable{
		Lb: &RouteLb{Names: []string{"b.example", "a.example", "b.example"}},
		Services: []*RouteService{
			{
				Name:            "one",
				Exposed:         true,
				Names:           []string{"d.example", "a.example", "c.example"},
				RedirectAliases: []*RouteRedirectAlias{{Name: "e.example", Target: "c.example"}},
			},
			{
				Name:            "two",
				Exposed:         true,
				Names:           []string{"c.example", "e.example"},
				RedirectAliases: []*RouteRedirectAlias{{Name: "d.example", Target: "c.example"}},
			},
		},
	}
	want := []string{"b.example", "a.example", "c.example", "d.example", "e.example"}
	if servedNames := routeTable.ServedNames(); !slices.Equal(servedNames, want) {
		t.Fatalf("ServedNames() = %v, want %v", servedNames, want)
	}
}

// The fixture's table with mcp unexposed. Its names move under
// hidden.example, which no fixture certificate covers, beside a wildcard
// longer than connect's *.connect.service.example, and it gets a redirect
// alias there.
func newTestUnexposedRouteTable(t *testing.T) *RouteTable {
	t.Helper()
	routeTable, err := ParseRouteTable(routeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	mcp, ok := routeTable.Service("mcp")
	if !ok {
		t.Fatal("no mcp service in the fixture")
	}
	mcp.Exposed = false
	mcp.Names = []string{"*.g1.connect.service.example", "mcp.hidden.example"}
	mcp.RedirectAliases = []*RouteRedirectAlias{
		{Name: "old-mcp.hidden.example", Target: "mcp.hidden.example"},
	}
	return routeTable
}

// A service that isn't exposed serves no name: nginx emits no server block
// for it, so neither its names nor its redirect aliases are listed, and none
// of them needs a certificate (LBALT-IMPL.md §7).
func TestRouteTableServedNamesLeavesOutAnUnexposedService(t *testing.T) {
	want := []string{
		"test-lb.service.example",
		"test-lb.other.example",
		"*.connect.service.example",
		"api.service.example",
		"connect.service.example",
		"old-api.service.example",
		"test-api.other.example",
		"test-api.service.example",
		"test-connect.other.example",
		"test-connect.service.example",
	}
	if servedNames := newTestUnexposedRouteTable(t).ServedNames(); !slices.Equal(servedNames, want) {
		t.Fatalf("ServedNames() = %v, want %v", servedNames, want)
	}
}

// The identity limit's fields are optional: the fixture has none, parses
// with the identity limit off, and encodes back to its own bytes (above), so
// a file warpctl renders without them keeps its revision. Written, they
// parse into their fields and encode back as written, in the schema's
// order after exclude_subnets.
func TestParseRouteTableReadsTheOptionalIdentityLimit(t *testing.T) {
	routeTable, err := ParseRouteTable(routeFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, routeInterface := range routeTable.Interfaces {
		rateLimit := routeInterface.RateLimit
		if rateLimit.IdentityRequestsPerMinute != 0 || rateLimit.IdentityBurst != 0 || rateLimit.IdentityAddressFactor != 0 {
			t.Fatalf("%s: identity fields %v without them in the file", routeInterface.Name, rateLimit)
		}
		if newLimitInterface(routeInterface).identityEnabled() {
			t.Fatalf("%s: the identity limit is on without its fields", routeInterface.Name)
		}
	}

	tree := routeFixtureTree(t)
	rateLimit := routeTreeValue(t, tree, "interfaces", 1, "rate_limit").(map[string]any)
	rateLimit["identity_requests_per_minute"] = json.Number("120")
	rateLimit["identity_burst"] = json.Number("60")
	rateLimit["identity_address_factor"] = json.Number("20")
	raw := append(encodeRouteTree(t, tree), '\n')
	routeTable, err = ParseRouteTable(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := RouteRateLimit{
		RequestsPerMinute:         450,
		Burst:                     150,
		NetConnections:            50,
		ExcludeSubnets:            []string{"10.0.0.0/8", "172.16.0.0/12"},
		IdentityRequestsPerMinute: 120,
		IdentityBurst:             60,
		IdentityAddressFactor:     20,
	}
	if got := *routeTable.Interfaces[1].RateLimit; !reflect.DeepEqual(got, want) {
		t.Fatalf("rate_limit = %#v, want %#v", got, want)
	}
	if routeTable.Interfaces[0].RateLimit.IdentityRequestsPerMinute != 0 {
		t.Fatal("another interface took the identity fields")
	}
	limits := newLimitInterface(routeTable.Interfaces[1])
	if !limits.identityEnabled() || limits.identityMilliRequestsPerSecond != 2000 || limits.identityBurst != 60 ||
		limits.ceilingMilliRequestsPerSecond != 7500*20 || limits.ceilingBurst != 150*20 || limits.ceilingNetConnections != 50*20 {
		t.Fatalf("limits %+v, want the identity at 120 r/m and the ceiling 20 times the address", limits)
	}

	// the fields encode after exclude_subnets, as warpctl writes them
	encoded, err := json.MarshalIndent(routeTable, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encodedText := string(encoded)
	order := []string{`"exclude_subnets"`, `"identity_requests_per_minute": 120`, `"identity_burst": 60`, `"identity_address_factor": 20`}
	last := -1
	for _, key := range order {
		i := strings.Index(encodedText, key)
		if i <= last {
			t.Fatalf("%s is out of order or missing in the encoding", key)
		}
		last = i
	}
	// and that encoding, as warpctl would write the file, parses to the same
	// table and encodes to the same bytes
	reparsed, err := ParseRouteTable(append(encoded, '\n'))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reparsed, routeTable) {
		t.Fatal("the encoded table with identity fields parses to another table")
	}
	reencoded, err := json.MarshalIndent(reparsed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reencoded, encoded) {
		t.Fatal("a table with identity fields doesn't encode back to its file")
	}
}

// An optional field may be absent, but never null.
func TestParseRouteTableRefusesANullOptionalField(t *testing.T) {
	for _, key := range []string{"identity_requests_per_minute", "identity_burst", "identity_address_factor"} {
		tree := routeFixtureTree(t)
		routeTreeValue(t, tree, "interfaces", 0, "rate_limit").(map[string]any)[key] = nil
		_, err := ParseRouteTable(encodeRouteTree(t, tree))
		fieldError := routeFieldErrorOf(err)
		if fieldPath := "interfaces[0].rate_limit." + key; fieldError == nil || fieldError.fieldPath != fieldPath || fieldError.message != "must not be null" {
			t.Errorf("null %s: err = %v, want null refused", key, err)
		}
	}
}

// Each identity limit rule refuses its field: none negative; off, no burst
// or factor; on, a factor from 1 to the cap, which can't make the ceiling's
// burst or cap overflow.
func TestParseRouteTableValidatesTheIdentityLimit(t *testing.T) {
	for _, c := range []struct {
		name      string
		values    map[string]any
		fieldPath string
		message   string
	}{
		{name: "a negative rate", values: map[string]any{"identity_requests_per_minute": -1, "identity_address_factor": 1}, fieldPath: "identity_requests_per_minute", message: "negative"},
		{name: "a negative burst", values: map[string]any{"identity_requests_per_minute": 60, "identity_burst": -1, "identity_address_factor": 1}, fieldPath: "identity_burst", message: "negative"},
		{name: "a negative factor", values: map[string]any{"identity_requests_per_minute": 60, "identity_address_factor": -1}, fieldPath: "identity_address_factor", message: "negative"},
		{name: "a burst while off", values: map[string]any{"identity_burst": 10}, fieldPath: "identity_burst", message: "zero without identity_requests_per_minute"},
		{name: "a factor while off", values: map[string]any{"identity_address_factor": 10}, fieldPath: "identity_address_factor", message: "zero without identity_requests_per_minute"},
		{name: "on without a factor", values: map[string]any{"identity_requests_per_minute": 60}, fieldPath: "identity_address_factor", message: "1-1000"},
		{name: "a factor over the cap", values: map[string]any{"identity_requests_per_minute": 60, "identity_address_factor": 1001}, fieldPath: "identity_address_factor", message: "1-1000"},
		{name: "a ceiling that overflows", values: map[string]any{"burst": json.Number("9223372036854775807"), "identity_requests_per_minute": 60, "identity_address_factor": 2}, fieldPath: "identity_address_factor", message: "overflow"},
	} {
		tree := routeFixtureTree(t)
		rateLimit := routeTreeValue(t, tree, "interfaces", 0, "rate_limit").(map[string]any)
		for key, value := range c.values {
			rateLimit[key] = value
		}
		_, err := ParseRouteTable(encodeRouteTree(t, tree))
		fieldError := routeFieldErrorOf(err)
		if fieldPath := "interfaces[0].rate_limit." + c.fieldPath; fieldError == nil || fieldError.fieldPath != fieldPath || !strings.Contains(fieldError.message, c.message) {
			t.Errorf("%s: err = %v, want %s refused: %s", c.name, err, fieldPath, c.message)
		}
	}

	// the smallest valid identity limit, and a fractional rate
	for _, values := range []map[string]any{
		{"identity_requests_per_minute": 1, "identity_address_factor": 1},
		{"identity_requests_per_minute": json.Number("0.5"), "identity_burst": 0, "identity_address_factor": 1000},
		{"identity_requests_per_minute": 0, "identity_burst": 0, "identity_address_factor": 0},
	} {
		tree := routeFixtureTree(t)
		rateLimit := routeTreeValue(t, tree, "interfaces", 0, "rate_limit").(map[string]any)
		for key, value := range values {
			rateLimit[key] = value
		}
		if _, err := ParseRouteTable(encodeRouteTree(t, tree)); err != nil {
			t.Errorf("%v: %v", values, err)
		}
	}
}
