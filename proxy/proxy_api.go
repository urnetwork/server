package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	// "fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

type apiServer struct {
	ctx                context.Context
	cancel             context.CancelFunc
	proxyDeviceManager *ProxyDeviceManager
	transportTls       *server.TransportTls
	settings           *ProxySettings
}

func newApiServer(
	ctx context.Context,
	cancel context.CancelFunc,
	proxyDeviceManager *ProxyDeviceManager,
	transportTls *server.TransportTls,
	settings *ProxySettings,
) *apiServer {
	s := &apiServer{
		ctx:                ctx,
		cancel:             cancel,
		proxyDeviceManager: proxyDeviceManager,
		transportTls:       transportTls,
		settings:           settings,
	}

	go server.HandleError(s.run, cancel)

	return s
}

func (self *apiServer) run() {
	defer self.cancel()

	routes := []*router.Route{
		router.NewRoute("POST", "/warmup", self.HandleWarmup),
	}

	reusePort := false

	httpServerOptions := server.HttpServerOptions{
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  5 * time.Minute,
	}

	tlsConfig := &tls.Config{
		GetConfigForClient: self.transportTls.GetTlsConfigForClient,
	}

	err := server.HttpListenAndServeTlsWithReusePort(
		self.ctx,
		net.JoinHostPort("", strconv.Itoa(ListenApiPort)),
		router.NewRouter(self.ctx, routes),
		reusePort,
		httpServerOptions,
		tlsConfig,
	)
	if err != nil {
		panic(err)
	}
}

type WarmupRequest struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

type WarmupResponse struct {
	Ready bool `json:"ready"`
}

func (self *apiServer) HandleWarmup(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	proxyId, err := authHeaderProxyId(authHeader)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var warmupRequest WarmupRequest

	defer r.Body.Close()
	bodyBytes, err := io.ReadAll(r.Body)

	if 0 < len(bodyBytes) {
		err = json.Unmarshal(bodyBytes, &warmupRequest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// else use the default object

	proxyDevice, err := self.proxyDeviceManager.OpenProxyDevice(proxyId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	timeout := time.Duration(warmupRequest.TimeoutSeconds) * time.Second
	ready := proxyDevice.WaitForReady(r.Context(), timeout)

	warmupResponse := &WarmupResponse{
		Ready: ready,
	}

	out, err := json.Marshal(warmupResponse)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(out)
}
