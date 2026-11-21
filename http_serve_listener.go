package server

// Serving on a listener the caller bound, for a server whose port the OS picks: a test's on 127.0.0.1:0, so several
// servers that bind a fixed port in production run side by side in one process.

import (
	"context"
	"net"
	"net/http"
)

// Serves handler on listener until ctx ends, then drains as HttpListenAndServeWithReusePort does (http_drain.go): the
// keepalive grace, then Shutdown within ShutdownTimeout. It takes listener and closes it on every return. A bound
// listener carries the caller's own socket options, so DisableTcpKeepAlive, a listen-time option, does not apply.
func HttpServeListener(ctx context.Context, listener net.Listener, handler http.Handler, httpServerOptions HttpServerOptions) error {
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer listener.Close()

	drainHandler := newDrainHandler(handler)
	tracker := newHttpConnTracker()

	server := &http.Server{
		Addr:         listener.Addr().String(),
		Handler:      drainHandler,
		ReadTimeout:  httpServerOptions.ReadTimeout,
		WriteTimeout: httpServerOptions.WriteTimeout,
		IdleTimeout:  httpServerOptions.IdleTimeout,
		ConnState:    tracker.connState,
	}

	return httpServeWithDrain(
		cancelCtx,
		cancel,
		server,
		drainHandler,
		tracker,
		func() error {
			return server.Serve(listener)
		},
		httpServerOptions,
	)
}
