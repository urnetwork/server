package mcp

// End to end coverage of the fetch tool over the real stack: an mcp client
// calls the tool over streamable http, the tool egresses through the real
// proxy ingress to a real provider, and the provider loads a local web server.
//
// The stack (connect server, api server, provider, proxy) is built by
// setupFetchTestStack. These tests thread the harness's existing signed proxy
// id so the tool takes its reuse path; provisioning a fresh proxy would need a
// second device to converge on the provider, which the harness already proves
// works and which the warmup gates make slow to reach by location query.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
)

const fetchTestOAuthClientId = "https://claude.ai/mcp"

// Points the fetch tool at the harness's local proxy ingress and loopback web
// server, and restores the production defaults afterwards.
func useFetchTestStack(stack *fetchTestStack) func() {
	fetchUsePlainHttpProxy = true
	fetchProxyAddrOverride = fmt.Sprintf("127.0.0.1:%d", stack.httpPort)

	return func() {
		fetchUsePlainHttpProxy = false
		fetchProxyAddrOverride = ""
	}
}

// An authorized mcp session against an in-process mcp server. The fetch tool
// needs both scopes: the transport enforces the read scope and the tool itself
// enforces the fetch scope.
func connectFetchTestClient(t testing.TB, ctx context.Context, stack *fetchTestStack) (*mcpsdk.ClientSession, func()) {
	serverUrl, cleanupServer := startTestServer(t)
	accessToken, _, err := oauth.MintAccessToken(&oauth.MintAccessTokenArgs{
		UserId:    stack.pdUserId,
		NetworkId: stack.pdNetworkId,
		ClientId:  fetchTestOAuthClientId,
		Audience:  McpResource,
		Scopes: []string{
			oauth.ScopeMcpRead,
			oauth.ScopeMcpFetch,
		},
	})
	if err != nil {
		t.Fatalf("mint fetch test access token: %v", err)
	}

	session := connectTestClientWithToken(t, ctx, serverUrl, accessToken)

	return session, func() {
		session.Close()
		cleanupServer()
	}
}

// Calls fetch, retrying while the proxy device converges on the provider. The
// first call after the stack comes up can beat the device's path to the
// provider, which surfaces as a transport error rather than a bad result.
func callFetchUntilOk(
	t testing.TB,
	ctx context.Context,
	session *mcpsdk.ClientSession,
	args map[string]any,
	timeout time.Duration,
) (*mcpsdk.CallToolResult, *FetchResult) {
	endTime := time.Now().Add(timeout)

	for {
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "fetch",
			Arguments: args,
		})
		connect.AssertEqual(t, err, nil)

		if !result.IsError {
			out := &FetchResult{}
			connect.AssertEqual(t, unmarshalStructured(t, result, out), nil)
			return result, out
		}

		if endTime.Before(time.Now()) {
			t.Fatalf("fetch did not succeed within %s: %s", timeout, errorText(result))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context done waiting for fetch")
		case <-time.After(2 * time.Second):
		}
	}
}

// Decodes a tool result's structured content into out. The client receives it
// as generic json, so it round trips through a re-marshal.
func unmarshalStructured(t testing.TB, result *mcpsdk.CallToolResult, out any) error {
	if result.StructuredContent == nil {
		t.Fatalf("the result carried no structured content")
	}
	structuredJson, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(structuredJson, out)
}

func errorText(result *mcpsdk.CallToolResult) string {
	texts := []string{}
	for _, content := range result.Content {
		if textContent, ok := content.(*mcpsdk.TextContent); ok {
			texts = append(texts, textContent.Text)
		}
	}
	return strings.Join(texts, " | ")
}

func TestFetchThroughProviderEgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		stack := setupFetchTestStack(t)
		defer stack.close()

		defer useFetchTestStack(stack)()

		session, cleanup := connectFetchTestClient(t, stack.ctx, stack)
		defer cleanup()

		result, out := callFetchUntilOk(t, stack.ctx, session, map[string]any{
			"url":             stack.webUrl + "/",
			"signed_proxy_id": stack.mcpSignedProxyId,
		}, 120*time.Second)

		connect.AssertEqual(t, out.Status, 200)
		// the egress handle comes back so the caller can reuse it
		connect.AssertEqual(t, out.SignedProxyId != "", true)
		handle := &sealedProxyHandle{}
		binding := identityStateBinding(
			stack.pdUserId.String(),
			stack.pdNetworkId,
			fetchTestOAuthClientId,
			McpResource,
		)
		connect.AssertEqual(t, unseal(sealLabelProxy, binding, out.SignedProxyId, handle), nil)
		connect.AssertEqual(t, handle.SignedProxyId, stack.signedProxyId)
		// and the caller is told what to do with it
		connect.AssertEqual(t, strings.Contains(out.NextStep, "signed_proxy_id"), true)

		// the page body reached the model as text
		body := ""
		for _, content := range result.Content {
			if textContent, ok := content.(*mcpsdk.TextContent); ok {
				if strings.Contains(textContent.Text, "URNETWORK_FETCH_TEST_PAGE") {
					body = textContent.Text
				}
			}
		}
		connect.AssertEqual(t, body != "", true)

		// static discovery found the referenced resources, listed by default
		kinds := map[string]bool{}
		for _, resource := range out.Resources {
			kinds[resource.Kind] = true
			connect.AssertEqual(t, resource.Embedded, false)
		}
		connect.AssertEqual(t, kinds[resourceKindImage], true)
		connect.AssertEqual(t, kinds[resourceKindStylesheet], true)
	})
}

func TestFetchEmbedsReferencedMedia(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		stack := setupFetchTestStack(t)
		defer stack.close()

		defer useFetchTestStack(stack)()

		session, cleanup := connectFetchTestClient(t, stack.ctx, stack)
		defer cleanup()

		result, out := callFetchUntilOk(t, stack.ctx, session, map[string]any{
			"url":               stack.webUrl + "/",
			"signed_proxy_id":   stack.mcpSignedProxyId,
			"include_resources": includeResourcesEmbed,
		}, 120*time.Second)

		embedded := 0
		for _, resource := range out.Resources {
			if resource.Embedded {
				embedded += 1
			}
		}
		connect.AssertEqual(t, 0 < embedded, true)

		// an image is delivered as a renderable image block, not base64 text,
		// so a vision model can actually see it
		imageBlocks := 0
		for _, content := range result.Content {
			if imageContent, ok := content.(*mcpsdk.ImageContent); ok {
				connect.AssertEqual(t, imageContent.MIMEType, "image/png")
				connect.AssertEqual(t, 0 < len(imageContent.Data), true)
				imageBlocks += 1
			}
		}
		connect.AssertEqual(t, 0 < imageBlocks, true)
	})
}

func TestFetchThreadsCookiesAcrossCalls(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		stack := setupFetchTestStack(t)
		defer stack.close()

		defer useFetchTestStack(stack)()

		session, cleanup := connectFetchTestClient(t, stack.ctx, stack)
		defer cleanup()

		// the first load sets a cookie, which comes back sealed
		_, out := callFetchUntilOk(t, stack.ctx, session, map[string]any{
			"url":               stack.webUrl + "/setcookie",
			"signed_proxy_id":   stack.mcpSignedProxyId,
			"include_resources": includeResourcesNone,
		}, 120*time.Second)

		connect.AssertEqual(t, out.Cookies != "", true)
		connect.AssertEqual(t, strings.Contains(out.NextStep, "cookies"), true)
		// the jar is opaque: the raw cookie value must not be readable
		connect.AssertEqual(t, strings.Contains(out.Cookies, "urtest"), false)

		// threading it back carries the session to the next call
		withResult, withCookies := callFetchUntilOk(t, stack.ctx, session, map[string]any{
			"url":               stack.webUrl + "/showcookie",
			"signed_proxy_id":   out.SignedProxyId,
			"cookies":           out.Cookies,
			"include_resources": includeResourcesNone,
		}, 120*time.Second)
		connect.AssertEqual(t, withCookies.Status, 200)
		connect.AssertEqual(t, strings.Contains(errorText(withResult), "cookie=1"), true)

		// and without it the site sees no session, which is what makes the
		// threading meaningful rather than incidental
		withoutResult, withoutCookies := callFetchUntilOk(t, stack.ctx, session, map[string]any{
			"url":               stack.webUrl + "/showcookie",
			"signed_proxy_id":   out.SignedProxyId,
			"include_resources": includeResourcesNone,
		}, 120*time.Second)
		connect.AssertEqual(t, withoutCookies.Status, 200)
		connect.AssertEqual(t, strings.Contains(errorText(withoutResult), "cookie=absent"), true)
	})
}

func TestFetchNonPublicTargetTimesOutThroughConnect(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		var targetRequestCount atomic.Int64
		stack := setupFetchTestStackWithOptions(t, &fetchTestStackOptions{
			onWebRequest: func() {
				targetRequestCount.Add(1)
			},
		})
		defer stack.close()
		defer useFetchTestStack(stack)()

		session, cleanup := connectFetchTestClient(t, stack.ctx, stack)
		defer cleanup()

		originalFetchCallBudget := fetchCallBudget
		fetchCallBudget = 1 * time.Second
		defer func() {
			fetchCallBudget = originalFetchCallBudget
		}()

		requestCountBeforeFetch := targetRequestCount.Load()
		startTime := time.Now()
		result, err := session.CallTool(stack.ctx, &mcpsdk.CallToolParams{
			Name: "fetch",
			Arguments: map[string]any{
				"url":               stack.webUrl + "/",
				"signed_proxy_id":   stack.mcpSignedProxyId,
				"include_resources": includeResourcesNone,
			},
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.IsError, true)
		connect.AssertEqual(t, 750*time.Millisecond <= time.Since(startTime), true)
		errorMessage := strings.ToLower(errorText(result))
		connect.AssertEqual(t,
			strings.Contains(errorMessage, "timeout") || strings.Contains(errorMessage, "deadline exceeded"),
			true,
		)
		connect.AssertEqual(t, targetRequestCount.Load(), requestCountBeforeFetch)
	})
}
