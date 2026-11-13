package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A request that failed before an answer returned the whole url in its error
// (`Get "https://host/path?query": cause`), and callers log those errors and
// store them: a Helius api key in the query reached the logs and
// payment_reconciliation_event, and the Play push's Google id token reached
// the logs. Each helper's error keeps the operation, the scheme and host, and
// the cause. The context is canceled before the call, so no request leaves the
// process. Every credential is synthetic.
func TestHttpRequestErrorsHoldNoUrlPathOrQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requestUrl := "https://synthetic-user:synthetic-password@synthetic-host.example:8443/v1/tokens/synthetic-path-token?api-key=synthetic-api-key&id_token=synthetic-id-token"
	credentials := []string{"synthetic-user", "synthetic-password", "synthetic-path-token", "synthetic-api-key", "synthetic-id-token", "/v1/tokens"}
	for _, c := range []struct {
		op   string
		call func() error
	}{
		{op: "Get", call: func() error {
			_, err := HttpGetRawRequireStatusOk(ctx, requestUrl, NoCustomHeaders)
			return err
		}},
		{op: "Post", call: func() error {
			_, err := HttpPostRawRequireStatusOk(ctx, requestUrl, []byte("{}"), NoCustomHeaders)
			return err
		}},
		{op: "Post", call: func() error {
			_, err := HttpPostForm(ctx, requestUrl, url.Values{"token": []string{"x"}}, NoCustomHeaders, func(response *http.Response, responseBodyBytes []byte) ([]byte, error) {
				return responseBodyBytes, nil
			})
			return err
		}},
		{op: "Delete", call: func() error {
			_, err := HttpDeleteBasic[map[string]any](ctx, requestUrl)
			return err
		}},
	} {
		err := c.call()
		if err == nil {
			t.Fatalf("%s: a canceled request succeeded", c.op)
		}
		for _, credential := range credentials {
			if strings.Contains(err.Error(), credential) {
				t.Errorf("%s: the error holds the url's %q: %s", c.op, credential, err)
			}
		}
		var urlErr *url.Error
		if !errors.As(err, &urlErr) || urlErr.Op != c.op || urlErr.URL != "https://synthetic-host.example:8443" {
			t.Errorf("%s: the error is %#v, want the operation and the scheme and host", c.op, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: the error lost its cause: %s", c.op, err)
		}
	}

	// a url that does not parse leaves out the url whole
	_, err := HttpGetRawRequireStatusOk(ctx, "https://synthetic-host.example/\x7f?api-key=synthetic-api-key", NoCustomHeaders)
	if err == nil || strings.Contains(err.Error(), "synthetic-api-key") || strings.Contains(err.Error(), "synthetic-host") {
		t.Errorf("an unparsed url's error is %v", err)
	}
}
