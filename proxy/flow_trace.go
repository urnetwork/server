package proxy

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/proxy/flowtrace"
)

const flowTracePath = "/diagnostics/flow-trace"

// A read never creates or warms a replacement device. Its recorder must be
// the one explicitly armed before this request; a lost generation is missing
// evidence, not an invitation to trace a new device.
func (m *ProxyDeviceManager) flowTraceDevice(id server.Id) *ProxyDevice {
	m.stateLock.Lock()
	state := m.proxyDevices[id]
	m.stateLock.Unlock()
	if state == nil {
		return nil
	}
	state.StateLock.Lock()
	defer state.StateLock.Unlock()
	return state.ProxyDevice
}

type flowTraceHandler struct {
	auth   func(string) (server.Id, error)
	open   func(server.Id) (*ProxyDevice, error)
	lookup func(server.Id) *ProxyDevice
}

// This endpoint has the same signed-proxy authentication as the existing
// control API and can observe only that caller's hosted device. Recording is
// off until explicitly armed, expires after 45 minutes, and retains at most
// 1024 metadata records. No packet bytes or raw provider identities are exposed.
func (h flowTraceHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	id, err := h.auth(request.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var snapshot flowtrace.Snapshot
	switch request.Method {
	case http.MethodPost:
		var start flowtrace.StartRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, request.Body, 1024)).Decode(&start); err != nil || start.Port == 0 {
			http.Error(w, "invalid trace target", http.StatusBadRequest)
			return
		}
		recorder, err := flowtrace.New(start.Port, time.Now())
		if err != nil {
			http.Error(w, "trace unavailable", http.StatusServiceUnavailable)
			return
		}
		device, err := h.open(id)
		if err != nil || device == nil {
			http.Error(w, "device unavailable", http.StatusServiceUnavailable)
			return
		}
		device.flowTrace.Store(recorder)
		snapshot = recorder.Cursor(time.Now())
	case http.MethodGet:
		device := h.lookup(id)
		if device == nil {
			http.Error(w, "trace unavailable", http.StatusGone)
			return
		}
		recorder := device.flowTrace.Load()
		if recorder == nil {
			http.Error(w, "trace unavailable", http.StatusGone)
			return
		}
		snapshot = recorder.Cursor(time.Now())
		if request.Header.Get("X-UR-Flow-Trace") != snapshot.Session || !snapshot.Now.Before(snapshot.Until) {
			http.Error(w, "trace generation unavailable", http.StatusGone)
			return
		}
		if request.URL.Query().Get("cursor_only") != "1" {
			after, err := strconv.ParseUint(request.URL.Query().Get("after"), 10, 64)
			if err != nil {
				http.Error(w, "invalid cursor", http.StatusBadRequest)
				return
			}
			afterDropped, err := strconv.ParseUint(request.URL.Query().Get("after_dropped"), 10, 64)
			if err != nil {
				http.Error(w, "invalid dropped cursor", http.StatusBadRequest)
				return
			}
			snapshot = recorder.Snapshot(after, afterDropped, time.Now())
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(snapshot)
}

func (p *ProxyDevice) recordFlowTraceDial(protocol string, connection net.Conn) {
	if recorder := p.flowTrace.Load(); recorder != nil {
		recorder.Dial(protocol, connection, time.Now())
	}
}
