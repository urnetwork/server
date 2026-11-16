// Bounded json rpc reads for the §22.6 through §22.9 subnet family readers.
// One family run owns one client and its exchange budget; each exchange is a
// single json rpc batch relayed to the pinned archive node through the
// existing ssh runner, so the shared per-host command limit and host
// exclusions apply unchanged. Source failures keep a fixed public reason and
// a private cause; raw responses, endpoints and errors never reach a finding.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// HostStdinSignalSource optionally lets a synthetic SignalSource receive the
// stdin of a host command. Production ssh supplies stdin natively; without
// this optional interface a synthetic source still refuses stdin.
type HostStdinSignalSource interface {
	HostStdin(ctx context.Context, host HostSettings, command string, stdin string, timeout time.Duration) (string, error)
}

// The relay reads only the selected loopback node port. Proxies, redirects
// and decompression are disabled; the remote read ends before its ssh owner.
// The json rpc body arrives on stdin and is never part of the command line.
const subnetReplayRelayTemplate = `# monitor-signal-22-subnet-replay
exec curl --silent --show-error --fail --noproxy '*' --max-time %d --max-filesize %d --header 'Content-Type: application/json' --data-binary @- http://127.0.0.1:%d/`

// Renders the fixed relay command. Only validated integers are interpolated.
func subnetReplayRelayCommand(rpcPort int, settings *SubnetReplaySettings) string {
	return fmt.Sprintf(subnetReplayRelayTemplate, int(settings.RpcRemoteReadTimeout/time.Second), settings.RpcMaximumResponseBytes, rpcPort)
}

// A fixed-reason observation gap. The cause stays inspectable with errors.Is
// and errors.As, while Error renders only the public reason.
type subnetReplayUnknown struct {
	reason string
	cause  error
}

func (self *subnetReplayUnknown) Error() string { return "subnet replay unknown: " + self.reason }

func (self *subnetReplayUnknown) Unwrap() error { return self.cause }

// Builds a gap with a private cause; a nil cause is allowed.
func newSubnetReplayUnknown(reason string, cause error) error {
	return &subnetReplayUnknown{reason: reason, cause: cause}
}

// Returns the public reason of a gap, or "" for any other error.
func subnetReplayUnknownReason(err error) string {
	var unknown *subnetReplayUnknown
	if errors.As(err, &unknown) {
		return unknown.reason
	}
	return ""
}

// One relay exchange to one node endpoint.
type subnetReplayRelay interface {
	exchange(ctx context.Context, request []byte) ([]byte, error)
}

// Relays through the probe runner, which is the production ssh runner, the
// host-scope policy runner, or a synthetic source with HostStdin.
type subnetReplaySshRelay struct {
	runner   probeRunner
	target   *host
	rpcPort  int
	settings *SubnetReplaySettings
}

func (self *subnetReplaySshRelay) exchange(ctx context.Context, request []byte) ([]byte, error) {
	command := subnetReplayRelayCommand(self.rpcPort, self.settings)
	output, err := self.runner.sshTimeout(ctx, self.target, command, string(request), self.settings.RpcExchangeTimeout)
	if err != nil {
		return nil, err
	}
	return []byte(output), nil
}

// A json rpc error object. Data carries revert bytes for a reverted eth_call.
type subnetReplayRpcFailure struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// One member of a relayed batch. A json null result leaves a pointer target
// nil; callers decide whether that is valid absence or missing evidence.
type subnetReplayRpcCall struct {
	method  string
	params  []any
	result  any
	failure *subnetReplayRpcFailure
}

// Owns one family run's relayed json rpc budget. Not safe for concurrent use.
type subnetReplayRpc struct {
	relay         subnetReplayRelay
	settings      *SubnetReplaySettings
	exchangeCount int
	callCount     int
}

func newSubnetReplayRpc(relay subnetReplayRelay, settings *SubnetReplaySettings) *subnetReplayRpc {
	return &subnetReplayRpc{relay: relay, settings: settings}
}

// Issues every call in as few bounded batches as the settings allow.
func (self *subnetReplayRpc) call(ctx context.Context, calls ...*subnetReplayRpcCall) error {
	batchSize := self.settings.RpcMaximumBatchRequests
	for start := 0; start < len(calls); start += batchSize {
		end := start + batchSize
		if len(calls) < end {
			end = len(calls)
		}
		if err := self.exchange(ctx, calls[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// Every member succeeds or the batch fails; per-member json rpc errors stay
// on the member so a caller can classify an expected revert.
func (self *subnetReplayRpc) exchange(ctx context.Context, calls []*subnetReplayRpcCall) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if len(calls) == 0 {
		return nil
	}
	if self.settings.RpcMaximumExchanges <= self.exchangeCount {
		return newSubnetReplayUnknown("rpc-budget-exhausted", nil)
	}
	self.exchangeCount += 1
	self.callCount += len(calls)
	type requestMember struct {
		Jsonrpc string `json:"jsonrpc"`
		Id      int    `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}
	members := make([]requestMember, len(calls))
	for i, call := range calls {
		params := call.params
		if params == nil {
			params = []any{}
		}
		members[i] = requestMember{Jsonrpc: "2.0", Id: i, Method: call.method, Params: params}
	}
	request, err := json.Marshal(members)
	if err != nil {
		return newSubnetReplayUnknown("rpc-request-invalid", err)
	}
	response, err := self.relay.exchange(ctx, request)
	select {
	case <-ctx.Done():
		return errors.Join(ctx.Err(), err)
	default:
	}
	if err != nil {
		return newSubnetReplayUnknown("rpc-relay-unavailable", err)
	}
	if self.settings.RpcMaximumResponseBytes < len(response) {
		return newSubnetReplayUnknown("rpc-response-oversized", nil)
	}
	type responseMember struct {
		Jsonrpc string                  `json:"jsonrpc"`
		Id      *int                    `json:"id"`
		Result  json.RawMessage         `json:"result"`
		Error   *subnetReplayRpcFailure `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(response))
	var received []responseMember
	if err := decoder.Decode(&received); err != nil {
		return newSubnetReplayUnknown("rpc-response-malformed", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return newSubnetReplayUnknown("rpc-response-malformed", err)
	}
	if len(received) != len(calls) {
		return newSubnetReplayUnknown("rpc-response-incomplete", nil)
	}
	seen := make([]bool, len(calls))
	for _, member := range received {
		if member.Jsonrpc != "2.0" || member.Id == nil || *member.Id < 0 || len(calls) <= *member.Id || seen[*member.Id] {
			return newSubnetReplayUnknown("rpc-response-malformed", nil)
		}
		seen[*member.Id] = true
		call := calls[*member.Id]
		resultPresent := len(member.Result) != 0
		if member.Error != nil {
			if resultPresent {
				return newSubnetReplayUnknown("rpc-response-malformed", nil)
			}
			failure := *member.Error
			call.failure = &failure
			continue
		}
		if !resultPresent {
			return newSubnetReplayUnknown("rpc-response-malformed", nil)
		}
		if call.result != nil {
			if err := json.Unmarshal(member.Result, call.result); err != nil {
				return newSubnetReplayUnknown("rpc-result-malformed", err)
			}
		}
	}
	return nil
}

// Returns a gap for the first member that failed, naming its method family.
func subnetReplayRpcFailed(calls ...*subnetReplayRpcCall) error {
	for _, call := range calls {
		if call.failure != nil {
			family, _, _ := strings.Cut(call.method, "_")
			return newSubnetReplayUnknown("rpc-"+family+"-failed", nil)
		}
	}
	return nil
}
