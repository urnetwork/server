package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
)

// The router enforces the no-cookies rule (session/cookies.go) for every
// route: a cookie header never reaches a handler, and one a handler adds never
// leaves, whichever way the handler commits its response.

func TestRouterDropsRequestCookies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := map[string]http.Header{}
	record := func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Clone()
	}
	handler := NewRouter(ctx, []*Route{
		NewRoute("GET", "/static", record),
		NewRoute("GET", "/capture/([^/]+)", record),
	})

	for _, path := range []string{"/static", "/capture/value", "/missing"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Add("Cookie", "session=secret")
		req.Header.Add("Cookie", "other=value")
		req.Header["cookie"] = []string{"lower=case"}
		req.Header.Set("Cookie2", `$Version="1"`)
		req.Header.Set("Authorization", "Bearer token")

		handler.ServeHTTP(httptest.NewRecorder(), req)

		requireOnlyAuthorization(t, path, req.Header)
	}
	for _, path := range []string{"/static", "/capture/value"} {
		header, ok := seen[path]
		if !ok {
			t.Fatalf("%s: the route did not run", path)
		}
		requireOnlyAuthorization(t, path, header)
	}
}

// Runs a real server, so every commit path is the native writer's: an
// explicit status, an implicit 200 from a write, a flush or a copy, an error,
// the router's recovery from a panic, and an informational status, which sends
// the header map once before the final status sends it again.
func TestRouterDropsResponseCookies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	setCookies := func(w http.ResponseWriter) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "secret", Domain: "bringyour.com"})
		w.Header()["set-cookie"] = []string{"lower=case"}
		w.Header()["Set-Cookie2"] = []string{"old=1"}
		w.Header().Set("X-Kept", "kept")
	}
	routes := []*Route{
		NewRoute("GET", "/write-header", func(w http.ResponseWriter, r *http.Request) {
			setCookies(w)
			w.WriteHeader(http.StatusCreated)
		}),
		NewRoute("GET", "/write", func(w http.ResponseWriter, r *http.Request) {
			setCookies(w)
			w.Write([]byte("ok"))
		}),
		NewRoute("GET", "/flush", func(w http.ResponseWriter, r *http.Request) {
			setCookies(w)
			w.(http.Flusher).Flush()
		}),
		NewRoute("GET", "/read-from", func(w http.ResponseWriter, r *http.Request) {
			setCookies(w)
			w.(io.ReaderFrom).ReadFrom(strings.NewReader("ok"))
		}),
		NewRoute("GET", "/error", func(w http.ResponseWriter, r *http.Request) {
			setCookies(w)
			http.Error(w, "refused", http.StatusBadRequest)
		}),
		NewRoute("GET", "/panic", func(w http.ResponseWriter, r *http.Request) {
			setCookies(w)
			panic("route failed")
		}),
		NewRoute("GET", "/informational", func(w http.ResponseWriter, r *http.Request) {
			setCookies(w)
			w.WriteHeader(http.StatusEarlyHints)
			setCookies(w)
			w.Write([]byte("ok"))
		}),
	}
	httpServer := httptest.NewServer(NewRouter(ctx, routes))
	defer httpServer.Close()

	for _, route := range routes {
		t.Run(route.pattern, func(t *testing.T) {
			informational := []textproto.MIMEHeader{}
			trace := &httptrace.ClientTrace{
				Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
					informational = append(informational, header)
					return nil
				},
			}
			req, err := http.NewRequestWithContext(
				httptrace.WithClientTrace(ctx, trace),
				"GET",
				httpServer.URL+route.pattern,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := httpServer.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}

			requireNoSetCookie(t, resp.Header)
			if resp.Header.Get("X-Kept") != "kept" {
				t.Fatalf("a header other than a cookie was dropped: %v", resp.Header)
			}
			for _, header := range informational {
				requireNoSetCookie(t, http.Header(header))
			}
			if wantInformational := route.pattern == "/informational"; wantInformational != (len(informational) == 1) {
				t.Fatalf("informational responses = %d", len(informational))
			}
		})
	}
}

// Fails unless header holds only the Authorization header.
func requireOnlyAuthorization(t *testing.T, path string, header http.Header) {
	t.Helper()
	if len(header) != 1 || header.Get("Authorization") != "Bearer token" {
		t.Fatalf("%s: header = %v, want only Authorization", path, header)
	}
}

// Fails if header holds a cookie-setting header in any letter case.
func requireNoSetCookie(t *testing.T, header http.Header) {
	t.Helper()
	for key, values := range header {
		if strings.EqualFold(key, "Set-Cookie") || strings.EqualFold(key, "Set-Cookie2") {
			t.Fatalf("%s survived: %v", key, values)
		}
	}
}
