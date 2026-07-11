package api

// spec_conformance_test.go guards parity between the published OpenAPI spec
// (connect/api/bringyour.yml) and the Go implementation that serves it.
//
// It is pure reflection + yaml parsing: no database, redis, or env is needed.
// Run with:
//
//	go test ./api/ -run 'TestSpec' -v
//
// Two properties are asserted:
//
//   - TestSpecConformance: for every registered endpoint, every property in the
//     spec's request schema exists (with a compatible type) in the Go argument
//     struct, and every property in the spec's response schema exists in the Go
//     result struct. This is a one-directional "SPEC ⊆ IMPL" check: the server
//     must faithfully accept/emit everything the spec promises. Extra impl
//     fields not in the spec are logged as drift, not failed.
//
//   - TestSpecRoutesImplemented: every spec path+method has a matching route in
//     Routes(). Extra impl routes (webhooks, txt, legacy aliases) are logged.
//
// Security/auth parity is intentionally out of scope (not reflectable).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// ----------------------------------------------------------------------------
// Registry: implemented endpoint -> Go request/response types.
//
// Built by reading api.go routes -> the handler in api/handlers -> the wrapped
// model.*/controller.* function -> its argument struct (input) and result
// struct (output). argType is nil when the endpoint takes no JSON body (GET
// endpoints, or endpoints whose args come from path/query params).
// ----------------------------------------------------------------------------

type specEndpoint struct {
	method     string
	path       string       // spec path form, e.g. "/log/{clientId}/upload"
	argType    reflect.Type // nil if no request body
	resultType reflect.Type // nil if no/opaque response
}

func rt(v any) reflect.Type { return reflect.TypeOf(v) }

func registry() []specEndpoint {
	return []specEndpoint{
		{"GET", "/stats/last-90", nil, rt(model.Stats{})},
		{"GET", "/stats/providers", nil, rt(model.StatsProvidersResult{})},
		{"POST", "/stats/providers-last-n", rt(model.StatsProvidersArgs{}), rt(model.StatsProvidersResult{})},
		{"POST", "/stats/provider-last-n", rt(model.StatsProviderArgs{}), rt(model.StatsProviderResult{})},
		{"POST", "/stats/providers-overview-last-n", rt(model.StatsProvidersOverviewArgs{}), rt(model.StatsProvidersOverviewResult{})},
		{"POST", "/stats/leaderboard", rt(controller.GetLeaderboardArgs{}), rt(model.LeaderboardResult{})},

		{"POST", "/auth/login", rt(model.AuthLoginArgs{}), rt(model.AuthLoginResult{})},
		{"POST", "/auth/login-with-password", rt(model.AuthLoginWithPasswordArgs{}), rt(model.AuthLoginWithPasswordResult{})},
		{"POST", "/auth/verify", rt(model.AuthVerifyArgs{}), rt(model.AuthVerifyResult{})},
		{"GET", "/auth/refresh", nil, rt(controller.RefreshTokenResult{})},
		{"POST", "/auth/verify-send", rt(controller.AuthVerifySendArgs{}), rt(controller.AuthVerifySendResult{})},
		{"POST", "/auth/password-reset", rt(controller.AuthPasswordResetArgs{}), rt(controller.AuthPasswordResetResult{})},
		{"POST", "/auth/password-set", rt(model.AuthPasswordSetArgs{}), rt(controller.AuthPasswordSetResult{})},
		{"POST", "/auth/network-check", rt(model.NetworkCheckArgs{}), rt(model.NetworkCheckResult{})},
		{"POST", "/auth/network-create", rt(model.NetworkCreateArgs{}), rt(model.NetworkCreateResult{})},
		{"POST", "/auth/network-delete", nil, rt(controller.NetworkRemoveResult{})},
		{"POST", "/auth/code-create", rt(model.AuthCodeCreateArgs{}), rt(model.AuthCodeCreateResult{})},
		{"POST", "/auth/code-login", rt(model.AuthCodeLoginArgs{}), rt(model.AuthCodeLoginResult{})},
		{"POST", "/auth/upgrade-guest", rt(model.UpgradeGuestArgs{}), rt(model.UpgradeGuestResult{})},
		{"POST", "/auth/upgrade-guest-existing", rt(model.UpgradeGuestExistingArgs{}), rt(model.UpgradeGuestExistingResult{})},

		{"POST", "/network/auth-client", rt(model.AuthNetworkClientArgs{}), rt(model.AuthNetworkClientResult{})},
		{"POST", "/network/remove-client", rt(model.RemoveNetworkClientArgs{}), rt(model.RemoveNetworkClientResult{})},
		{"GET", "/network/clients", nil, rt(model.NetworkClientsResult{})},
		{"GET", "/network/peers", nil, rt(model.NetworkPeersResult{})},
		{"GET", "/network/provider-locations", nil, rt(model.FindLocationsResult{})},
		{"POST", "/network/find-provider-locations", rt(model.FindLocationsArgs{}), rt(model.FindLocationsResult{})},
		{"POST", "/network/find-providers2", rt(model.FindProviders2Args{}), rt(model.FindProviders2Result{})}, {"GET", "/network/ranking", nil, rt(controller.GetNetworkRankingResult{})},
		{"POST", "/network/ranking-visibility", rt(controller.SetNetworkRankingPublicArgs{}), rt(controller.SetNetworkRankingPublicResult{})},
		{"POST", "/network/block-location", rt(controller.NetworkBlockLocationArgs{}), rt(controller.NetworkBlockLocationResult{})},
		{"POST", "/network/unblock-location", rt(controller.NetworkUnblockLocationArgs{}), rt(controller.NetworkUnblockLocationResult{})},
		{"GET", "/network/blocked-locations", nil, rt(controller.GetNetworkBlockedLocationsResult{})},
		{"GET", "/network/reliability", nil, rt(controller.GetNetworkReliabilityResult{})},
		{"GET", "/network/user", nil, rt(controller.GetNetworkUserResult{})},
		{"POST", "/network/user/update", rt(controller.UpdateNetworkNameArgs{}), rt(controller.UpdateNetworkNameResult{})},

		{"POST", "/preferences/set-preferences", rt(model.AccountPreferencesSetArgs{}), rt(model.AccountPreferencesSetResult{})},
		{"GET", "/preferences", nil, rt(model.AccountPreferencesGetResult{})},
		{"POST", "/feedback/send-feedback", rt(model.FeedbackSendArgs{}), rt(model.FeedbackSendResult{})},
		{"POST", "/log/{clientId}/upload", nil, rt(controller.UploadLogFileResult{})},

		{"GET", "/wallet/balance", nil, rt(controller.WalletBalanceResult{})},
		{"POST", "/wallet/validate-address", rt(controller.WalletValidateAddressArgs{}), rt(controller.WalletValidateAddressResult{})},
		{"POST", "/wallet/circle-init", nil, rt(controller.WalletCircleInitResult{})},
		{"POST", "/wallet/circle-transfer-out", rt(controller.WalletCircleTransferOutArgs{}), rt(controller.WalletCircleTransferOutResult{})},

		{"GET", "/subscription/balance", nil, rt(controller.SubscriptionBalanceResult{})},
		{"POST", "/subscription/check-balance-code", rt(model.CheckBalanceCodeArgs{}), rt(model.CheckBalanceCodeResult{})},
		{"POST", "/subscription/redeem-balance-code", rt(controller.RedeemBalanceCodeArgs{}), rt(model.RedeemBalanceCodeResult{})},
		{"POST", "/subscription/create-payment-id", rt(model.SubscriptionCreatePaymentIdArgs{}), rt(model.SubscriptionCreatePaymentIdResult{})},

		{"POST", "/device/add", rt(model.DeviceAddArgs{}), rt(model.DeviceAddResult{})},
		{"POST", "/device/create-share-code", rt(model.DeviceCreateShareCodeArgs{}), rt(model.DeviceCreateShareCodeResult{})},
		{"POST", "/device/share-status", rt(model.DeviceShareStatusArgs{}), rt(model.DeviceShareStatusResult{})},
		{"POST", "/device/confirm-share", rt(model.DeviceConfirmShareArgs{}), rt(model.DeviceConfirmShareResult{})},
		{"POST", "/device/create-adopt-code", rt(model.DeviceCreateAdoptCodeArgs{}), rt(model.DeviceCreateAdoptCodeResult{})},
		{"POST", "/device/adopt-status", rt(model.DeviceAdoptStatusArgs{}), rt(model.DeviceAdoptStatusResult{})},
		{"POST", "/device/confirm-adopt", rt(model.DeviceConfirmAdoptArgs{}), rt(model.DeviceConfirmAdoptResult{})},
		{"POST", "/device/remove-adopt-code", rt(model.DeviceRemoveAdoptCodeArgs{}), rt(model.DeviceRemoveAdoptCodeResult{})},
		{"GET", "/device/associations", nil, rt(model.DeviceAssociationsResult{})},
		{"POST", "/device/remove-association", rt(model.DeviceRemoveAssociationArgs{}), rt(model.DeviceRemoveAssociationResult{})},
		{"POST", "/device/set-association-name", rt(model.DeviceSetAssociationNameArgs{}), rt(model.DeviceSetAssociationNameResult{})},
		{"POST", "/device/set-name", rt(model.DeviceSetNameArgs{}), rt(model.DeviceSetNameResult{})},
		{"POST", "/connect/control", rt(controller.ConnectControlArgs{}), rt(controller.ConnectControlResult{})},
		// no JSON body (303 SSO redirect) — presence-only, covered by hasOperation
		{"GET", "/connect", nil, nil},
		{"POST", "/connect", nil, nil},
		// oneOf request (VerifySeed|VerifyExtend); VerifyArgs is the superset.
		// Response is a polymorphic interface{}, so it stays unchecked (nil).
		{"POST", "/verify", rt(controller.VerifyArgs{}), nil},
		{"GET", "/key/{clientId}", nil, rt(controller.GetClientKeyResult{})},
		{"GET", "/hello", nil, rt(controller.HelloResult{})},

		{"POST", "/account/api-key", rt(model.CreateApiKeyArgs{}), rt(model.CreateApiKeyResult{})},
		{"POST", "/account/api-key/remove", rt(controller.DeleteApiKeyArgs{}), rt(controller.DeleteApiKeyResult{})},
		{"GET", "/account/api-keys", nil, rt(controller.GetApiKeysResult{})},
		{"POST", "/account/payout-wallet", rt(model.SetPayoutWalletArgs{}), rt(model.SetPayoutWalletResult{})},
		{"GET", "/account/payout-wallet", nil, rt(controller.GetPayoutWalletResult{})},
		{"GET", "/account/points", nil, rt(controller.AccountPointsResult{})},
		{"GET", "/account/payments", nil, rt(controller.GetNetworkAccountPaymentsResult{})},
		{"POST", "/account/wallet", rt(model.CreateAccountWalletExternalArgs{}), rt(model.CreateAccountWalletResult{})},
		{"GET", "/account/wallets", nil, rt(model.GetAccountWalletsResult{})},
		{"POST", "/account/wallets/remove", rt(model.RemoveWalletArgs{}), rt(model.RemoveWalletResult{})},
		{"POST", "/account/wallets/verify-seeker", rt(controller.VerifySeekerNftHolderArgs{}), rt(controller.VerifySeekerNftHolderResult{})},
		{"GET", "/account/balance-codes", nil, rt(controller.GetNetworkRedeemedBalanceCodesResult{})},
		{"GET", "/account/referral-code", nil, rt(controller.NetworkReferralResult{})},
		{"POST", "/referral-code/validate", rt(controller.ValidateReferralCodeArgs{}), rt(controller.ValidateNetworkReferralCodeResult{})},
		{"GET", "/account/referral-network", nil, rt(controller.GetNetworkReferralResult{})},
		{"GET", "/account/unlink-referral-network", nil, rt(controller.UnlinkReferralNetworkResult{})},
		{"POST", "/account/set-referral", rt(controller.SetNetworkReferralArgs{}), rt(controller.SetNetworkReferralResult{})},

		{"GET", "/transfer/stats", nil, rt(model.TransferStats{})},
		{"POST", "/solana/payment-intent", rt(controller.SolanaPaymentIntentArgs{}), rt(controller.SolanaPaymentIntentResult{})},
		{"POST", "/stripe/payment-intent", rt(controller.StripeCreatePaymentIntentArgs{}), rt(controller.StripeCreatePaymentIntentResult{})},
		{"POST", "/stripe/customer-portal", rt(controller.StripeCreateCustomerPortalArgs{}), rt(controller.StripeCreateCustomerPortalResult{})},

		{"GET", "/verify/keys", nil, rt(controller.GetVerifyKeysResult{})},
		{"POST", "/sn/wallet", rt(controller.SnSetWalletArgs{}), rt(controller.SnSetWalletResult{})},
		{"GET", "/sn/pool/claim", nil, rt(controller.SnPoolClaimResult{})},
		{"GET", "/sn/epoch", nil, rt(model.StEpochSummary{})},
	}
}

// skips are matched endpoints whose request/response body cannot be reflected
// against the spec. They are logged, not failed. (They are still expected to be
// present as routes; TestSpecRoutesImplemented covers that.)
func skips() map[string]string {
	return map[string]string{
		"GET /my-ip-info":                      "response is an unexported handler-local struct (handlers.response)",
		"GET /device/share-code/{code}/qr.png": "image/png response, no JSON schema",
		"GET /device/adopt-code/{code}/qr.png": "image/png response, no JSON schema",
	}
}

// driftAllow lists impl json field names that are intentionally emitted but are
// not in the spec (dual-emit deprecated aliases). They are not reported as drift.
var driftAllow = map[string]bool{
	"wallet_login":   true,
	"account_points": true,
	"share_code":     true,
}

const maxDepth = 4

// ----------------------------------------------------------------------------
// Spec parsing
// ----------------------------------------------------------------------------

type specDoc struct {
	paths   map[string]any
	schemas map[string]any
}

func loadSpec(t *testing.T) *specDoc {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	specPath := filepath.Join(dir, "..", "..", "connect", "api", "bringyour.yml")

	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %s", specPath, err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %s", specPath, err)
	}
	components := asMap(root["components"])
	return &specDoc{
		paths:   asMap(root["paths"]),
		schemas: asMap(components["schemas"]),
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// resolve follows a chain of $ref into components.schemas.
func (s *specDoc) resolve(sch map[string]any) map[string]any {
	for i := 0; i < 16 && sch != nil; i++ {
		ref, ok := sch["$ref"].(string)
		if !ok {
			return sch
		}
		name := ref[strings.LastIndex(ref, "/")+1:]
		sch = asMap(s.schemas[name])
	}
	return sch
}

// properties returns the merged property map of a schema, resolving $ref and
// flattening allOf. Keys are json property names; values are the (unresolved)
// property schemas.
func (s *specDoc) properties(sch map[string]any) map[string]any {
	sch = s.resolve(sch)
	out := map[string]any{}
	if sch == nil {
		return out
	}
	// merge allOf, plus oneOf/anyOf (union of variant properties — the Go
	// request struct is expected to be a superset of all variants, e.g. /verify).
	for _, key := range []string{"allOf", "oneOf", "anyOf"} {
		for _, member := range asSlice(sch[key]) {
			for k, v := range s.properties(asMap(member)) {
				out[k] = v
			}
		}
	}
	for k, v := range asMap(sch["properties"]) {
		out[k] = v
	}
	return out
}

// kind returns the spec type of a schema: string/integer/number/boolean/array/
// object, or "" when it cannot be determined (e.g. bare oneOf/anyOf).
func (s *specDoc) kind(sch map[string]any) string {
	sch = s.resolve(sch)
	if sch == nil {
		return ""
	}
	if t, ok := sch["type"].(string); ok {
		return t
	}
	if _, ok := sch["allOf"]; ok {
		return "object"
	}
	if _, ok := sch["properties"]; ok {
		return "object"
	}
	return ""
}

// operation returns the request and response(200) JSON schemas for a spec
// path+method, or nil when absent.
func (s *specDoc) operation(method, path string) (req, resp map[string]any) {
	methods := asMap(s.paths[path])
	op := asMap(methods[strings.ToLower(method)])
	if op == nil {
		return nil, nil
	}
	if rb := asMap(op["requestBody"]); rb != nil {
		req = asMap(asMap(asMap(rb["content"])["application/json"])["schema"])
	}
	responses := asMap(op["responses"])
	if r200 := asMap(responses["200"]); r200 != nil {
		resp = asMap(asMap(asMap(r200["content"])["application/json"])["schema"])
	}
	return req, resp
}

// hasOperation reports whether the spec declares this path+method at all — even
// when it has no JSON request/response body (e.g. a redirect).
func (s *specDoc) hasOperation(method, path string) bool {
	return asMap(asMap(s.paths[path])[strings.ToLower(method)]) != nil
}

// ----------------------------------------------------------------------------
// Go reflection
// ----------------------------------------------------------------------------

// typeName renders a reflect.Type for messages, dereferencing pointers so the
// underlying struct name is shown (e.g. "model.AuthLoginArgs").
func typeName(t reflect.Type) string {
	if t == nil {
		return "<nil>"
	}
	return deref(t).String()
}

func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// goFields walks the exported fields of a struct type, honoring json tags and
// flattening embedded/anonymous structs. Returns json name -> field type.
func goFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	t = deref(t)
	if t == nil || t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := ""
		if tag := f.Tag.Get("json"); tag != "" {
			name = strings.Split(tag, ",")[0]
		}
		if name == "-" {
			continue
		}
		// Embedded struct with no json name: promote (flatten) its fields.
		if f.Anonymous && name == "" {
			if ft := deref(f.Type); ft != nil && ft.Kind() == reflect.Struct {
				for k, v := range goFields(ft) {
					if _, exists := out[k]; !exists {
						out[k] = v
					}
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*interface {
		MarshalText() ([]byte, error)
	})(nil)).Elem()
)

// marshalsToString reports whether t (or *t) has custom JSON/text marshaling,
// which for our types (server.Id, time.Time, ...) means a JSON string.
func marshalsToString(t reflect.Type) bool {
	if t == nil {
		return false
	}
	cands := []reflect.Type{t}
	if t.Kind() != reflect.Ptr {
		cands = append(cands, reflect.PointerTo(t))
	}
	for _, c := range cands {
		if c.Implements(jsonMarshalerType) || c.Implements(textMarshalerType) {
			return true
		}
	}
	return false
}

// kindCompatible reports whether a Go type can carry a spec value of specKind.
func kindCompatible(specKind string, t reflect.Type) bool {
	orig := t
	t = deref(t)
	if t == nil {
		return specKind == "object" // interface{} etc.
	}
	k := t.Kind()
	switch specKind {
	case "string":
		if k == reflect.String {
			return true
		}
		if k == reflect.Slice && t.Elem().Kind() == reflect.Uint8 { // []byte -> base64 string
			return true
		}
		// time.Time, server.Id, and other string-marshaling types
		return marshalsToString(t) || marshalsToString(orig)
	case "integer":
		switch k {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return true
		}
		return false
	case "number":
		return k == reflect.Float32 || k == reflect.Float64
	case "boolean":
		return k == reflect.Bool
	case "array":
		return k == reflect.Slice || k == reflect.Array
	case "object":
		return k == reflect.Struct || k == reflect.Map || k == reflect.Interface
	}
	return true // unknown spec kind: don't fail
}

// ----------------------------------------------------------------------------
// TestSpecConformance
// ----------------------------------------------------------------------------

type conformanceChecker struct {
	t    *testing.T
	spec *specDoc
}

// check asserts SPEC ⊆ IMPL for one (spec schema, Go type) pair and recurses
// into nested objects/arrays up to maxDepth. It also logs impl fields that are
// absent from the spec (drift).
func (c *conformanceChecker) check(endpoint, prefix string, specSch map[string]any, goType reflect.Type, depth int) {
	specProps := c.spec.properties(specSch)
	if len(specProps) == 0 {
		return
	}
	goFlds := goFields(goType)

	// SPEC ⊆ IMPL: every spec property must exist with a compatible type.
	for name, propAny := range specProps {
		prop := asMap(propAny)
		specKind := c.spec.kind(prop)
		gt, ok := goFlds[name]
		if !ok {
			c.t.Errorf("[%s] spec field %q (spec kind %q) is missing in Go type %s",
				endpoint, prefix+name, specKind, typeName(goType))
			continue
		}
		if specKind != "" && !kindCompatible(specKind, gt) {
			c.t.Errorf("[%s] field %q: spec kind %q is incompatible with Go type %s",
				endpoint, prefix+name, specKind, gt)
			continue
		}
		if depth >= maxDepth || specKind == "" {
			continue
		}
		switch specKind {
		case "object":
			resolved := c.spec.resolve(prop)
			if len(c.spec.properties(resolved)) > 0 {
				if et := deref(gt); et != nil && et.Kind() == reflect.Struct {
					c.check(endpoint, prefix+name+".", resolved, et, depth+1)
				}
			}
		case "array":
			items := asMap(c.spec.resolve(prop)["items"])
			if items != nil && c.spec.kind(items) == "object" && len(c.spec.properties(items)) > 0 {
				if et := deref(gt); et != nil && (et.Kind() == reflect.Slice || et.Kind() == reflect.Array) {
					if elem := deref(et.Elem()); elem != nil && elem.Kind() == reflect.Struct {
						c.check(endpoint, prefix+name+"[].", items, elem, depth+1)
					}
				}
			}
		}
	}

	// Drift (informational): impl fields absent from the spec.
	for name, gt := range goFlds {
		if _, ok := specProps[name]; ok {
			continue
		}
		if driftAllow[name] {
			continue
		}
		c.t.Logf("[drift] %s: impl field %q (%s) is not in the spec", endpoint, prefix+name, gt)
	}
}

func TestSpecConformance(t *testing.T) {
	spec := loadSpec(t)
	checker := &conformanceChecker{t: t, spec: spec}

	skip := skips()
	covered := 0

	for _, ep := range registry() {
		endpoint := ep.method + " " + ep.path
		if !spec.hasOperation(ep.method, ep.path) {
			t.Errorf("[%s] endpoint is in the registry but not found in the spec", endpoint)
			continue
		}
		req, resp := spec.operation(ep.method, ep.path)
		if ep.argType != nil && req != nil {
			checker.check(endpoint+" (request)", "", req, ep.argType, 0)
		}
		if ep.resultType != nil && resp != nil {
			checker.check(endpoint+" (response)", "", resp, ep.resultType, 0)
		}
		covered++
	}

	// Log the intentional skips with their reasons.
	skipKeys := make([]string, 0, len(skip))
	for k := range skip {
		skipKeys = append(skipKeys, k)
	}
	sort.Strings(skipKeys)
	for _, k := range skipKeys {
		t.Logf("[skip] %s: %s", k, skip[k])
	}

	t.Logf("spec conformance: %d endpoints covered, %d skipped", covered, len(skip))
}

// ----------------------------------------------------------------------------
// TestSpecRoutesImplemented
// ----------------------------------------------------------------------------

var (
	reRegexCapture = regexp.MustCompile(`\(\[\^/\]\+\)`)
	reSpecParam    = regexp.MustCompile(`\{[^/}]+\}`)
)

// normalizePath collapses both Go regex capture groups and OpenAPI path params
// to a common "{}" token so the two route forms can be compared.
func normalizePath(p string) string {
	p = strings.TrimPrefix(p, "^")
	p = strings.TrimSuffix(p, "$")
	p = reRegexCapture.ReplaceAllString(p, "{}")
	p = reSpecParam.ReplaceAllString(p, "{}")
	return p
}

// implRouteKeys returns the normalized "METHOD path" keys for every route.
// Route exposes method+pattern only via String() ("METHOD ^pattern$").
func implRouteKeys() map[string]bool {
	out := map[string]bool{}
	for _, r := range Routes() {
		id := r.String() // e.g. "GET ^/hello$"
		method, pattern, ok := strings.Cut(id, " ")
		if !ok {
			continue
		}
		out[method+" "+normalizePath(pattern)] = true
	}
	return out
}

func TestSpecRoutesImplemented(t *testing.T) {
	spec := loadSpec(t)
	implKeys := implRouteKeys()

	specKeys := map[string]bool{}
	var missing []string
	for path, methodsAny := range spec.paths {
		for method := range asMap(methodsAny) {
			switch method {
			case "get", "post", "put", "delete", "patch":
			default:
				continue
			}
			key := strings.ToUpper(method) + " " + normalizePath(path)
			specKeys[key] = true
			if !implKeys[key] {
				missing = append(missing, key)
			}
		}
	}

	sort.Strings(missing)
	for _, k := range missing {
		t.Errorf("spec endpoint %q has no matching route in Routes()", k)
	}

	// Impl routes absent from the spec (webhooks, txt, legacy aliases): logged.
	var extra []string
	for k := range implKeys {
		if !specKeys[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		t.Logf("[route not in spec] %s", k)
	}

	t.Logf("spec routes: %d spec endpoints, %d impl routes, %d spec endpoints unmatched, %d impl routes not in spec",
		len(specKeys), len(implKeys), len(missing), len(extra))
}
