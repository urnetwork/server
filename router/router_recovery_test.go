package router

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server"
)

// hijackedResponseWriter is a deterministic net/http ownership boundary. Once
// Hijack succeeds, any later ResponseWriter call is invalid because the route
// owns the connection directly.
type hijackedResponseWriter struct {
	header              http.Header
	hijacked            bool
	writesAfterHijack   int
	statusesAfterHijack []int
}

func newHijackedResponseWriter() *hijackedResponseWriter {
	return &hijackedResponseWriter{header: make(http.Header)}
}

func (w *hijackedResponseWriter) Header() http.Header {
	return w.header
}

func (w *hijackedResponseWriter) WriteHeader(status int) {
	if w.hijacked {
		w.writesAfterHijack++
		w.statusesAfterHijack = append(w.statusesAfterHijack, status)
	}
}

func (w *hijackedResponseWriter) Write(body []byte) (int, error) {
	if w.hijacked {
		w.writesAfterHijack++
	}
	return len(body), nil
}

func (w *hijackedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, nil
}

// TestRouterDonePanicDoesNotWriteAfterHijack reproduces the live Connect
// sequence: the GET / route has already handed its H1 socket to Gorilla, then
// a canceled database operation raises the standard Done panic while the
// connection is closing. Recovery must consume that expected lifecycle signal
// without trying to synthesize an HTTP 500 on a connection net/http no longer
// owns.
func TestRouterDonePanicDoesNotWriteAfterHijack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, []*Route{NewRoute("GET", "/", func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Fatalf("hijack: %v", err)
		}
		panic(server.DbContextDoneError)
	})})
	w := newHijackedResponseWriter()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if !w.hijacked {
		t.Fatal("test route did not cross the hijack ownership boundary")
	}
	if w.writesAfterHijack != 0 {
		t.Fatalf("router attempted %d response writes after hijack (statuses %v)", w.writesAfterHijack, w.statusesAfterHijack)
	}
}

func TestRouterUnexpectedPanicStillWritesInternalServerError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, []*Route{NewRoute("GET", "/", func(http.ResponseWriter, *http.Request) {
		panic(errors.New("synthetic unexpected route failure"))
	})})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected panic status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if body := w.Body.String(); body != "Error. Please email support@ur.io for help.\n" {
		t.Fatalf("unexpected panic body = %q", body)
	}
}
