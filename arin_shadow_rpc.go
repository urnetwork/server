package server

// This protocol is for an explicitly enabled, protected operator capture. It
// has no public HTTP route, retry, address field, automatic credential loading,
// or policy activation. A reviewed pipe/Unix bridge carries only finite private
// keys and classification facts; the final operator output is an aggregate.
import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"
)

const ArinShadowRPCRequestLimit = 64 << 10
const ArinShadowRPCResponseLimit = 256 << 10
const ArinShadowRPCMaxCalls = 16384
const ArinShadowRPCCallTimeout = 5 * time.Second

type ArinShadowRPCIdentity struct {
	ProcessNonce Id        `json:"process_nonce"`
	StartedAt    time.Time `json:"started_at"`
	Revision     string    `json:"revision"`
	Modified     bool      `json:"modified"`
	ImageDigest  string    `json:"image_digest"`
	Role         string    `json:"role"`
	Version      string    `json:"version"`
	Environment  string    `json:"environment"`
	Host         string    `json:"host"`
	Block        string    `json:"block"`
}

type ArinShadowRPCRequest struct {
	RunId        Id              `json:"run_id"`
	Nonce        Id              `json:"nonce"`
	ProcessNonce Id              `json:"process_nonce"`
	Deadline     time.Time       `json:"deadline"`
	Method       string          `json:"method"`
	Input        json.RawMessage `json:"input"`
}

type arinShadowRPCReply struct {
	RunId         Id                    `json:"run_id"`
	Nonce         Id                    `json:"nonce"`
	RequestSHA256 string                `json:"request_sha256"`
	Identity      ArinShadowRPCIdentity `json:"identity"`
	At            time.Time             `json:"at"`
	OK            bool                  `json:"ok"`
	Output        json.RawMessage       `json:"output"`
}

type ArinShadowRPCHandler func(context.Context, string, json.RawMessage) (any, error)

type ArinShadowRPCService struct {
	ctx      context.Context
	runId    Id
	key      [32]byte
	identity ArinShadowRPCIdentity
	handle   ArinShadowRPCHandler
	mu       sync.Mutex
	seen     map[Id]bool
	active   bool
}

func NewArinShadowRPCService(ctx context.Context, runId Id, key [32]byte, identity ArinShadowRPCIdentity, handle ArinShadowRPCHandler) (*ArinShadowRPCService, error) {
	if ctx == nil || ctx.Err() != nil || runId == (Id{}) || key == ([32]byte{}) || handle == nil || identity.ProcessNonce == (Id{}) || identity.StartedAt.IsZero() ||
		(identity.Role != "connect" && identity.Role != "native") || len(identity.Revision) != 40 || !arinShadowHex(identity.Revision, 20) {
		return nil, ErrArinShadowInput
	}
	return &ArinShadowRPCService{ctx: ctx, runId: runId, key: key, identity: identity, handle: handle, seen: map[Id]bool{}}, nil
}

func arinShadowHex(value string, size int) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == size
}

func DecodeArinShadowRPC(data []byte, output any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(output) != nil {
		return ErrArinShadowInput
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrArinShadowInput
	}
	return nil
}

func arinShadowSign(key [32]byte, data []byte) []byte {
	h := hmac.New(sha256.New, key[:])
	h.Write(data)
	return append(h.Sum(nil), data...)
}

func arinShadowVerify(key [32]byte, packet []byte, limit int) ([]byte, error) {
	if len(packet) <= sha256.Size || len(packet) > limit {
		return nil, ErrArinShadowInput
	}
	data := packet[sha256.Size:]
	h := hmac.New(sha256.New, key[:])
	h.Write(data)
	if !hmac.Equal(packet[:sha256.Size], h.Sum(nil)) {
		return nil, ErrArinShadowInput
	}
	return data, nil
}

// Handle rejects authentication, replay, resource and byte/clock bounds before
// any owning callback. At most one callback per service executes concurrently.
// Refusal is finite and carries no callback error, SQL, key or address text.
func (s *ArinShadowRPCService) Handle(ctx context.Context, packet []byte) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || s.ctx.Err() != nil {
		return nil, ErrArinShadowInput
	}
	data, err := arinShadowVerify(s.key, packet, ArinShadowRPCRequestLimit)
	if err != nil {
		return nil, err
	}
	var request ArinShadowRPCRequest
	now := NowUtc()
	if DecodeArinShadowRPC(data, &request) != nil || request.RunId != s.runId || request.Nonce == (Id{}) || request.Deadline.Before(now) || request.Deadline.After(now.Add(ArinShadowRPCCallTimeout+arinShadowCaptureClockSkew)) ||
		(request.Method != "hello" && request.ProcessNonce != s.identity.ProcessNonce) {
		return nil, ErrArinShadowInput
	}
	s.mu.Lock()
	if s.active || s.seen[request.Nonce] || len(s.seen) >= ArinShadowRPCMaxCalls {
		s.mu.Unlock()
		return nil, ErrArinShadowInput
	}
	s.active = true
	s.seen[request.Nonce] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active = false; s.mu.Unlock() }()
	bounded, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	sum := sha256.Sum256(data)
	reply := arinShadowRPCReply{RunId: s.runId, Nonce: request.Nonce, RequestSHA256: hex.EncodeToString(sum[:]), Identity: s.identity}
	var output any
	if request.Method == "hello" {
		output = struct{}{}
	} else {
		output, err = s.handle(bounded, request.Method, request.Input)
	}
	if err == nil && bounded.Err() == nil {
		reply.Output, err = json.Marshal(output)
		reply.OK = err == nil
	}
	reply.At = NowUtc()
	encoded, err := json.Marshal(reply)
	if err != nil || len(encoded)+sha256.Size > ArinShadowRPCResponseLimit || bounded.Err() != nil {
		return nil, ErrArinShadowInput
	}
	return arinShadowSign(s.key, encoded), nil
}

// RoundTrip must honor context, transfer one bounded request/reply, and never
// retry. Unix and reviewed SSH stdin bridges implement this same byte contract.
type ArinShadowRPCRoundTrip func(context.Context, []byte) ([]byte, error)

// A protected bridge uses only this destination to select a socket from its
// explicit inventory. It has no key and grants no authenticity: the endpoint
// still verifies the entire HMAC and exact process nonce before doing work.
func ArinShadowRPCDestination(packet []byte) (Id, error) {
	if len(packet) <= sha256.Size || len(packet) > ArinShadowRPCRequestLimit {
		return Id{}, ErrArinShadowInput
	}
	var request ArinShadowRPCRequest
	if DecodeArinShadowRPC(packet[sha256.Size:], &request) != nil || request.ProcessNonce == (Id{}) {
		return Id{}, ErrArinShadowInput
	}
	return request.ProcessNonce, nil
}

type ArinShadowRPCClient struct {
	runId     Id
	key       [32]byte
	identity  ArinShadowRPCIdentity
	transport ArinShadowRPCRoundTrip
	permit    chan struct{}
}

func NewArinShadowRPCClient(runId Id, key [32]byte, expected ArinShadowRPCIdentity, transport ArinShadowRPCRoundTrip) (*ArinShadowRPCClient, error) {
	if runId == (Id{}) || key == ([32]byte{}) || expected.ProcessNonce == (Id{}) || transport == nil || expected.StartedAt.IsZero() || !arinShadowHex(expected.Revision, 20) {
		return nil, ErrArinShadowInput
	}
	return &ArinShadowRPCClient{runId: runId, key: key, identity: expected, transport: transport, permit: make(chan struct{}, 1)}, nil
}

func (c *ArinShadowRPCClient) Identity() ArinShadowRPCIdentity { return c.identity }

func (c *ArinShadowRPCClient) Call(ctx context.Context, method string, input, output any) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrArinShadowInput
	}
	bounded, cancel := context.WithTimeout(ctx, ArinShadowRPCCallTimeout)
	defer cancel()
	select {
	case c.permit <- struct{}{}:
		defer func() { <-c.permit }()
	case <-bounded.Done():
		return ErrArinShadowInput
	}
	deadline, _ := bounded.Deadline()
	encoded, err := json.Marshal(input)
	if err != nil {
		return ErrArinShadowInput
	}
	request := ArinShadowRPCRequest{RunId: c.runId, Nonce: NewId(), ProcessNonce: c.identity.ProcessNonce, Deadline: deadline, Method: method, Input: encoded}
	data, err := json.Marshal(request)
	if err != nil || len(data)+sha256.Size > ArinShadowRPCRequestLimit {
		return ErrArinShadowInput
	}
	packet, err := c.transport(bounded, arinShadowSign(c.key, data))
	if err != nil || bounded.Err() != nil {
		return ErrArinShadowInput
	}
	raw, err := arinShadowVerify(c.key, packet, ArinShadowRPCResponseLimit)
	if err != nil {
		return err
	}
	var reply arinShadowRPCReply
	sum := sha256.Sum256(data)
	if DecodeArinShadowRPC(raw, &reply) != nil || reply.RunId != c.runId || reply.Nonce != request.Nonce || reply.RequestSHA256 != hex.EncodeToString(sum[:]) || reply.Identity != c.identity || !reply.OK || reply.At.Before(deadline.Add(-ArinShadowRPCCallTimeout-arinShadowCaptureClockSkew)) || reply.At.After(NowUtc().Add(arinShadowCaptureClockSkew)) {
		return ErrArinShadowInput
	}
	return DecodeArinShadowRPC(reply.Output, output)
}

func ReadArinShadowRPCFrame(r io.Reader, limit int) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, ErrArinShadowInput
	}
	n := binary.BigEndian.Uint32(header[:])
	if n <= sha256.Size || n > uint32(limit) {
		return nil, ErrArinShadowInput
	}
	data := make([]byte, int(n))
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, ErrArinShadowInput
	}
	return data, nil
}

func WriteArinShadowRPCFrame(w io.Writer, data []byte, limit int) error {
	if len(data) <= sha256.Size || len(data) > limit {
		return ErrArinShadowInput
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := io.Copy(w, bytes.NewReader(header[:])); err != nil {
		return ErrArinShadowInput
	}
	if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
		return ErrArinShadowInput
	}
	return nil
}

func ArinShadowUnixRoundTrip(path string) ArinShadowRPCRoundTrip {
	return func(ctx context.Context, request []byte) ([]byte, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			return nil, ErrArinShadowInput
		}
		defer connection.Close()
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, ErrArinShadowInput
		}
		connection.SetDeadline(deadline)
		stop := context.AfterFunc(ctx, func() { connection.Close() })
		defer stop()
		if WriteArinShadowRPCFrame(connection, request, ArinShadowRPCRequestLimit) != nil {
			return nil, ErrArinShadowInput
		}
		return ReadArinShadowRPCFrame(connection, ArinShadowRPCResponseLimit)
	}
}
