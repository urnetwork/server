package mcp

// The fetch tool speaks the x402 MCP transport (x402-foundation/x402
// specs/transports-v2/mcp.md): a plan limit that needs a payment is a tool
// error whose structured content is the PaymentRequired and whose first text
// content is its JSON, the payment comes back in _meta["x402/payment"], and a
// failed payment is answered the way payment required is, with the reason as
// the terms' error. Until this change the terms sat one level down, in
// payment_required, the first text content was the refusal's message, and
// only a payment argument was read, so no x402 MCP client could find the
// terms or send its payment (reference clients: x402-foundation/x402
// typescript/packages/mcp x402MCPClient extractPaymentRequiredFromResult,
// go/mcp/client.go; Cloudflare agents withX402Client).
//
// Pure: no database, no facilitator.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urnetwork/server/v2026/controller"
)

// Terms for a Pro month as the controller quotes them, in both versions.
func fetchX402TestTerms() *controller.X402Terms {
	extra := map[string]any{"name": "USD Coin", "version": "2"}
	payTo := "0x" + strings.Repeat("b1", 20)
	asset := "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	return controller.Testing_NewX402Terms(
		&controller.X402PaymentRequired{
			X402Version: 1,
			Error:       "limit reached. UR Pro, 1 month costs $5.00 in USDC.",
			Accepts: []controller.X402Accept{{
				Scheme: "exact", Network: "base", MaxAmountRequired: "5000000",
				Resource: fetchX402Resource, Description: "UR Pro, 1 month", MimeType: "application/json",
				PayTo: payTo, Asset: asset, MaxTimeoutSeconds: 300, Extra: extra,
			}},
		},
		&controller.X402PaymentRequiredV2{
			X402Version: 2,
			Error:       "limit reached. UR Pro, 1 month costs $5.00 in USDC.",
			Resource:    controller.X402ResourceInfo{Url: fetchX402Resource, Description: "UR Pro, 1 month", MimeType: "application/json"},
			Accepts: []controller.X402AcceptV2{{
				Scheme: "exact", Network: "eip155:8453", Amount: "5000000",
				Asset: asset, PayTo: payTo, MaxTimeoutSeconds: 300, Extra: extra,
			}},
		},
		"UR Pro, 1 month",
		5,
	)
}

// fetchX402TestStructured is the structured content the go-sdk sends for a
// typed result: the output's JSON, validated against the tool's output
// schema as mcp.AddTool derives it (jsonschema.ForType, then Resolve).
func fetchX402TestStructured(t *testing.T, out *FetchResult) map[string]any {
	t.Helper()
	schema, err := jsonschema.For[FetchResult](&jsonschema.ForOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	structured := map[string]any{}
	if err := json.Unmarshal(encoded, &structured); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(structured); err != nil {
		t.Fatalf("the structured result does not fit the fetch tool's output schema: %v", err)
	}
	return structured
}

// requireFetchX402PaymentRequired checks a result the way an x402 MCP client
// reads one: isError, then structuredContent with x402Version and accepts,
// else content[0].text as the same JSON.
func requireFetchX402PaymentRequired(t *testing.T, result *mcpsdk.CallToolResult, out *FetchResult) (map[string]any, map[string]any) {
	t.Helper()
	if !result.IsError {
		t.Fatal("a payment required result is not a tool error")
	}
	structured := fetchX402TestStructured(t, out)
	if structured["x402Version"] != float64(2) {
		t.Errorf("structured x402Version = %v, want 2", structured["x402Version"])
	}
	accepts, _ := structured["accepts"].([]any)
	if len(accepts) != 1 || accepts[0].(map[string]any)["network"] != "eip155:8453" {
		t.Errorf("structured accepts = %v", structured["accepts"])
	}
	if resource, _ := structured["resource"].(map[string]any); resource["url"] != "mcp://tool/fetch" {
		t.Errorf("structured resource = %v", structured["resource"])
	}
	if len(result.Content) == 0 {
		t.Fatal("no content")
	}
	first, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want text", result.Content[0])
	}
	text := map[string]any{}
	if err := json.Unmarshal([]byte(first.Text), &text); err != nil {
		t.Fatalf("content[0].text is not the PaymentRequired's JSON: %q", first.Text)
	}
	for _, field := range []string{"x402Version", "error", "resource", "accepts"} {
		if !reflect.DeepEqual(text[field], structured[field]) {
			t.Errorf("content[0].text %s = %v, structured %s = %v", field, text[field], field, structured[field])
		}
	}
	return structured, text
}

// A plan limit with x402 on answers with the transport's PaymentRequired, in
// v2, with the v1 terms kept as payment_required, and says the price and how
// to let a default client pay it.
func TestFetchUpgradeResultIsTheX402McpPaymentRequired(t *testing.T) {
	result, out, err := fetchUpgradeResult(&upgradeRequired{
		message: "Your plan's concurrent client limit is reached.",
		terms:   fetchX402TestTerms(),
	})
	if err != nil {
		t.Fatal(err)
	}
	structured, _ := requireFetchX402PaymentRequired(t, result, out)

	v1, _ := structured["payment_required"].(map[string]any)
	if v1["x402Version"] != float64(1) {
		t.Errorf("payment_required = %v, want the v1 terms", structured["payment_required"])
	}
	for _, says := range []string{"$5.00", "over $1", `_meta["x402/payment"]`, "PAYMENT-SIGNATURE"} {
		if !strings.Contains(out.NextStep, says) {
			t.Errorf("next step %q does not say %q", out.NextStep, says)
		}
	}
	// the refusal's message is still in the content, after the terms
	found := false
	for _, content := range result.Content[1:] {
		if text, ok := content.(*mcpsdk.TextContent); ok && text.Text == "Your plan's concurrent client limit is reached." {
			found = true
		}
	}
	if !found {
		t.Error("the refusal's message is not in the content")
	}

	// x402 off: the plain refusal, with no terms
	result, out, _ = fetchUpgradeResult(&upgradeRequired{message: "Your plan's concurrent client limit is reached."})
	if !result.IsError || out.X402Version != 0 || out.Accepts != nil || out.PaymentRequired != nil {
		t.Errorf("a refusal without terms: %+v", out)
	}
	fetchX402TestStructured(t, out)
}

// A payment the terms or the facilitator refused is answered as payment
// required again, with the reason as the terms' error (the x402 code in v2,
// the agent-facing text in v1); anything else is a plain tool error.
func TestFetchPaymentFailedResultQuotesTheTermsAgain(t *testing.T) {
	result, out, err := fetchPaymentFailedResult(&controller.X402PaymentError{
		Status:  402,
		Message: "x402 payment invalid: insufficient_funds",
		Reason:  "insufficient_funds",
		Terms:   fetchX402TestTerms(),
	})
	if err != nil {
		t.Fatal(err)
	}
	structured, text := requireFetchX402PaymentRequired(t, result, out)
	if structured["error"] != "insufficient_funds" || text["error"] != "insufficient_funds" {
		t.Errorf("the terms' error = %v, want insufficient_funds", structured["error"])
	}
	if out.PaymentRequired == nil || out.PaymentRequired.Error != "x402 payment invalid: insufficient_funds" {
		t.Errorf("payment_required = %+v", out.PaymentRequired)
	}
	if !strings.Contains(out.NextStep, "nothing was charged") {
		t.Errorf("next step %q", out.NextStep)
	}

	pending, out, _ := fetchPaymentFailedResult(&controller.X402PaymentError{
		Status:  402,
		Message: "x402 settlement pending: transaction 0x01 was broadcast",
		Reason:  "settlement_pending",
		Terms:   fetchX402TestTerms(),
	})
	if !pending.IsError || !strings.Contains(out.NextStep, "Do not sign another payment") {
		t.Errorf("a pending settlement: %q", out.NextStep)
	}

	for name, failure := range map[string]error{
		"a malformed payment": &controller.X402PaymentError{Status: 400, Message: "x402 payment is not base64"},
		"a facilitator error": &controller.X402PaymentError{Status: 502, Message: "x402 verify failed"},
		"x402 off":            errors.New("payments are not enabled"),
	} {
		result, out, err := fetchPaymentFailedResult(failure)
		if err != nil || out != nil || !result.IsError {
			t.Errorf("%s: %v %+v %v", name, result, out, err)
			continue
		}
		text := result.Content[0].(*mcpsdk.TextContent).Text
		if text != "The payment could not be settled: "+failure.Error() {
			t.Errorf("%s: %q", name, text)
		}
	}
}

// The payment arrives in _meta["x402/payment"] as the PaymentPayload object
// (what an x402 MCP client sends), or in the payment argument as the header
// form or as JSON text; it reaches the controller as base64 of the JSON
// either way. Both at once is refused.
func TestFetchPaymentReadsTheX402McpMeta(t *testing.T) {
	payload := map[string]any{
		"x402Version": float64(2),
		"accepted":    map[string]any{"scheme": "exact", "network": "eip155:8453"},
		"payload":     map[string]any{"signature": "0x01"},
	}
	payloadJson, _ := json.Marshal(payload)
	header := base64.StdEncoding.EncodeToString(payloadJson)

	request := func(meta mcpsdk.Meta) *mcpsdk.CallToolRequest {
		return &mcpsdk.CallToolRequest{Params: &mcpsdk.CallToolParamsRaw{Name: "fetch", Meta: meta}}
	}
	decode := func(t *testing.T, payment string) map[string]any {
		t.Helper()
		raw, err := base64.StdEncoding.DecodeString(payment)
		if err != nil {
			t.Fatalf("%q is not base64", payment)
		}
		decoded := map[string]any{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}

	payment, err := fetchPayment(request(mcpsdk.Meta{"x402/payment": payload}), FetchArgs{})
	if err != nil || !reflect.DeepEqual(decode(t, payment), payload) {
		t.Errorf("_meta payment: %q %v", payment, err)
	}
	payment, err = fetchPayment(request(nil), FetchArgs{Payment: header})
	if err != nil || payment != header {
		t.Errorf("header-form argument: %q %v", payment, err)
	}
	payment, err = fetchPayment(request(nil), FetchArgs{Payment: string(payloadJson)})
	if err != nil || !reflect.DeepEqual(decode(t, payment), payload) {
		t.Errorf("JSON argument: %q %v", payment, err)
	}
	if payment, err := fetchPayment(request(mcpsdk.Meta{"progressToken": "1"}), FetchArgs{}); err != nil || payment != "" {
		t.Errorf("no payment: %q %v", payment, err)
	}
	if _, err := fetchPayment(request(mcpsdk.Meta{"x402/payment": payload}), FetchArgs{Payment: header}); err == nil {
		t.Error("a payment in both places was taken")
	}
	if payment, err := fetchPayment(nil, FetchArgs{}); err != nil || payment != "" {
		t.Errorf("no request: %q %v", payment, err)
	}
}
