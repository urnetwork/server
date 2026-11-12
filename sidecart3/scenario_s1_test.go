//go:build sidecar_localstack

// S1 on T3 (§22.7; F1, I-6, I-7, D-TRUST-1), its three clauses, each its own test, on server's own routes, which a
// fake api proves nothing about:
//
//   - code-create-403: a sidecar's token, which api mints strict_exp and admin_refused through POST
//     /network/auth-client as a sidecar's enrollment does, is refused POST /auth/code-create with 403 "A sidecar token
//     cannot call this route." by server's own router, and the refused request stores nothing: the canary in its body
//     reaches no object, Redis key or sidecar table.
//   - token-never-in-mcp-env-or-logs: a sidecar enrolled through POST /network/enroll from an auth key the owner made
//     through POST /network/auth-keys, Online on server's exchange (T3Sidecar), then an MCP session against it as `ur
//     mcp` relays one to the daemon, every read-only tool called; every credential api answered or was sent (the token,
//     the auth key, the network's credential) is looked for in the session's results, the process environment, the
//     sidecar's own log, server's log at verbosity 2 and every store, and found in none.
//   - genesis-other-than-the-invites-refused: the owner's first device, enrolled the same way, signs the Genesis and
//     base 1 and posts them through POST /network/config/projection (BootstrapNetwork) to server's sequencer; a device
//     enrolled from an invite naming another genesis records that anchor, and its ConfigClient, reading GET
//     /network/config/projection with its own token, refuses the log; one enrolled from an invite naming the network's
//     genesis and base 1 follows it to base 1.
//
// Each runs under the runner's harness and guard, with the canary of its one user field: a refused request's body, and
// the config repo's policy text, which only the device's config repo holds.
//
// What the scenario stands in for, each named where it is: the config repo is sidecartest's ref in memory, since T3
// hosts no evs for a sidecar's pushes yet; the projection and publish calls go through this file's shim of the two
// routes with the device's own token, since a Sidecar's SidecarApi is the core's alone; and the ConfigClient is built
// here as a daemon's host would build it from the sidecar's anchor, since no host builds one yet.

package sidecart3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mcp"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// One request through server's router, as api's listener would serve it.
func serveApi(handler http.Handler, method string, path string, authorization string, body any) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		server.Raise(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	// RFC 5737's documentation range
	request.RemoteAddr = "198.51.100.71:4000"
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestScenarioS1CodeCreateOnT3(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		RunScenario(t, &sidecartest.Scenario{Id: "S1", Run: func(ctx context.Context, run *sidecartest.ScenarioRun) error {
			apiCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handler := ApiRouter(apiCtx)
			networkId := server.NewId()
			userId := server.NewId()
			networkName := fmt.Sprintf("s1%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
			model.Testing_CreateNetwork(apiCtx, networkId, networkName, userId)
			defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId))()
			root := "Bearer " + session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign()

			// the sidecar's client, marked as its enrollment marks it
			minted := serveApi(handler, http.MethodPost, "/network/auth-client", root, map[string]any{
				"description": "sidecar", "device_spec": "sidecar", "admin_refused": true, "strict_exp": true,
			})
			if minted.Code != http.StatusOK {
				return fmt.Errorf("auth-client: %d %s", minted.Code, minted.Body.String())
			}
			result := &model.AuthNetworkClientResult{}
			if err := json.Unmarshal(minted.Body.Bytes(), result); err != nil {
				return err
			}
			if result.ByClientJwt == nil || !result.StrictExp || !result.AdminRefused {
				return fmt.Errorf("the client is not a sidecar's: %s", minted.Body.String())
			}
			run.Net.Capture().AddSecret("a sidecar's client token", []byte(*result.ByClientJwt))

			// code-create with the sidecar's token: 403, never 401, and nothing of the request kept
			refused := serveApi(handler, http.MethodPost, "/auth/code-create", "Bearer "+*result.ByClientJwt, map[string]any{
				"uses": 1, "duration_minutes": 5, "description": run.CanaryText("a code's description"),
			})
			if refused.Code != http.StatusForbidden || strings.TrimSpace(refused.Body.String()) != router.StrictExpCredentialRefusedMessage {
				return fmt.Errorf("code-create with a sidecar's token: %d %q", refused.Code, refused.Body.String())
			}
			// the same route with the network's own credential is the route's own, so the 403 is the token's
			if allowed := serveApi(handler, http.MethodPost, "/auth/code-create", root, map[string]any{"uses": 1, "duration_minutes": 5}); allowed.Code != http.StatusOK {
				return fmt.Errorf("code-create with the network's credential: %d %s", allowed.Code, allowed.Body.String())
			}
			// server's own table holds the route off the allowlist, and the scenario's gate is that table
			if ServerRouteAllowed(http.MethodPost, "/auth/code-create") || !ServerRouteAllowed(http.MethodGet, "/auth/refresh") {
				return fmt.Errorf("server's route table and §7.4 part at /auth/code-create or /auth/refresh")
			}
			return nil
		}}, nil)
	})
}

// One S1 network on T3: a personal network in preview_networks, its owner's credential, server's api in process with
// server's sequencer serving its log from the Net's first metadata cluster, and server's exchange for its sidecars.
type s1Network struct {
	run       *sidecartest.ScenarioRun
	networkId server.Id
	userId    server.Id
	root      string
	api       *T3Api
	exchange  *T3SidecarExchange
	closes    []func()
}

func newS1Network(ctx context.Context, t testing.TB, run *sidecartest.ScenarioRun) (*s1Network, error) {
	self := &s1Network{run: run, networkId: server.NewId(), userId: server.NewId()}
	networkName := fmt.Sprintf("s1%s", strings.ReplaceAll(self.networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, self.networkId, networkName, self.userId)
	self.closes = append(self.closes, server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", self.networkId)))
	cluster, err := run.Net.Host().Blobs.FirstCluster()
	if err != nil {
		self.close()
		return nil, err
	}
	model.SetConfigLogSequencer(model.NewConfigLogSequencer(cluster, model.DefaultConfigLogSettings(), nil))
	self.closes = append(self.closes, func() { model.SetConfigLogSequencer(nil) })
	self.root = "Bearer " + session.NewByJwt(self.networkId, self.userId, networkName, false, false).Testing_Sign()
	self.api = NewT3Api(ctx, run.Net, nil)
	exchange, err := NewT3SidecarExchange(ctx, t, nil)
	if err != nil {
		self.close()
		return nil, err
	}
	self.exchange = exchange
	self.closes = append(self.closes, exchange.Close)
	return self, nil
}

// Closes what the network opened, newest first: its sidecars before the exchange they dial.
func (self *s1Network) close() {
	for index := len(self.closes) - 1; 0 <= index; index -= 1 {
		self.closes[index]()
	}
	self.closes = nil
}

// A one-use auth key the owner makes through POST /network/auth-keys.
func (self *s1Network) authKey(ctx context.Context) (string, error) {
	status, answer, err := self.api.Call(ctx, http.MethodPost, "/network/auth-keys", self.root, &model.CreateAuthKeyArgs{DurationSeconds: 600, MaxUses: 1})
	if err != nil {
		return "", err
	}
	result := &model.CreateAuthKeyResult{}
	if status != http.StatusOK || json.Unmarshal(answer, result) != nil || result.AuthKey == "" {
		return "", fmt.Errorf("auth-keys: %d", status)
	}
	return result.AuthKey, nil
}

// A sidecar enrolled from a fresh auth key of the network and anchor, nil for none, Online on server's exchange.
func (self *s1Network) enroll(ctx context.Context, name string, anchor *sidecar.SidecarEnrollAnchor) (*T3Sidecar, error) {
	authKey, err := self.authKey(ctx)
	if err != nil {
		return nil, err
	}
	device, err := StartT3Sidecar(ctx, self.run.Net, name, self.api, self.exchange, nil)
	if err != nil {
		return nil, err
	}
	self.closes = append(self.closes, device.Close)
	if err := device.EnrollOnline(ctx, &sidecar.SidecarEnrollCredential{AuthKey: authKey, Anchor: anchor}); err != nil {
		return nil, err
	}
	return device, nil
}

// The token device presents, as api minted it.
func (self *s1Network) bearer(device *T3Sidecar) (string, error) {
	identity := device.Sidecar().Identity()
	if identity == nil {
		return "", fmt.Errorf("%s holds no identity", device.Name)
	}
	token := self.api.ClientToken(identity.ClientId)
	if token == "" {
		return "", fmt.Errorf("api minted no token %s holds", device.Name)
	}
	return "Bearer " + token, nil
}

// GET and POST /network/config/projection as a device sends them with its own token: sidecar.ConfigProjectionReader and
// ConfigBasePublisher over T3Api, decoding what SidecarApi decodes (config_client.go, config_writer.go).
type t3ConfigRoutes struct {
	api    *T3Api
	bearer string
}

func (self *t3ConfigRoutes) ReadConfigProjection(ctx context.Context, after uint64) (*sidecar.ConfigProjection, error) {
	path := "/network/config/projection?" + url.Values{"after": []string{strconv.FormatUint(after, 10)}}.Encode()
	status, answer, err := self.api.Call(ctx, http.MethodGet, path, self.bearer, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /network/config/projection: %d", status)
	}
	reply := &struct {
		Genesis []byte   `json:"genesis"`
		Entries [][]byte `json:"entries"`
		Rows    [][]byte `json:"rows"`
		Tip     *struct {
			N     uint64 `json:"n"`
			Chain []byte `json:"chain"`
		} `json:"tip"`
		More bool `json:"more"`
	}{}
	if err := json.Unmarshal(answer, reply); err != nil {
		return nil, err
	}
	projection := &sidecar.ConfigProjection{SignedGenesis: reply.Genesis, Entries: reply.Entries, Rows: reply.Rows, More: reply.More}
	if reply.Tip != nil {
		projection.Tip = &bridge.AuthPin{Version: reply.Tip.N, Chain: reply.Tip.Chain}
	}
	return projection, nil
}

func (self *t3ConfigRoutes) PublishConfigBase(ctx context.Context, base *sidecar.ConfigBasePublish) (*bridge.AuthPin, error) {
	status, answer, err := self.api.Call(ctx, http.MethodPost, "/network/config/projection", self.bearer, map[string]any{
		"signed_header": base.SignedHeader, "rows": base.Rows, "genesis": base.Genesis, "config_pushes": base.ConfigPushes,
	})
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", sidecar.ErrConfigTipMoved, answer)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", sidecar.ErrConfigRefused, answer)
	default:
		return nil, fmt.Errorf("POST /network/config/projection: %d %s", status, answer)
	}
	reply := &struct {
		N     uint64 `json:"n"`
		Chain []byte `json:"chain"`
	}{}
	if err := json.Unmarshal(answer, reply); err != nil {
		return nil, err
	}
	return &bridge.AuthPin{Version: reply.N, Chain: reply.Chain}, nil
}

// The daemon one T3 sidecar's MCP server reads: its one network.
type s1McpDaemon struct {
	networkId sidecar.Id
	network   mcp.McpNetwork
}

func (self *s1McpDaemon) ResolveNetwork(hello *sidecar.DaemonHello, network string) (sidecar.Id, error) {
	if network != "" && network != self.networkId.String() {
		return sidecar.Id{}, &sidecar.DaemonNetworkError{Reason: "unknown"}
	}
	return self.networkId, nil
}

func (self *s1McpDaemon) Network(networkId sidecar.Id) (mcp.McpNetwork, bool) {
	return self.network, networkId == self.networkId
}

func (self *s1McpDaemon) Agent(hello *sidecar.DaemonHello) (*sidecar.SidecarAgent, error) {
	return nil, nil
}

// A session's stream whose every byte the server writes is captured as an MCP result (CC4).
type s1McpStream struct {
	io.ReadWriter
	results io.Writer
}

func (self *s1McpStream) Write(p []byte) (int, error) {
	self.results.Write(p)
	return self.ReadWriter.Write(p)
}

// An MCP session against device, as `ur mcp` relays one to the daemon: the daemon's catalog on device's settings, the
// server's every answer captured, and each read-only tool called with no arguments, a refusal being an answer too.
// Answers how many tools were called.
func runS1McpSession(ctx context.Context, run *sidecartest.ScenarioRun, device *T3Sidecar) (int, error) {
	identity := device.Sidecar().Identity()
	if identity == nil {
		return 0, fmt.Errorf("%s holds no identity", device.Name)
	}
	mcpServer := mcp.NewMcpServer(&s1McpDaemon{networkId: identity.NetworkId, network: device.Sidecar()}, mcp.NewMcpServerSettings(device.Settings, sidecar.Version))
	mcp.NewDaemonMcpCatalog(mcpServer, nil).Install(mcpServer)
	serverConn, clientConn := net.Pipe()
	sessionCtx, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		hello := &sidecar.DaemonHello{Mode: sidecar.DaemonModeMcp, Version: sidecar.Version, Harness: "claude-code"}
		served <- mcpServer.Serve(sessionCtx, hello, &s1McpStream{ReadWriter: serverConn, results: run.Net.Capture().Writer(sidecartest.PrivacyClassMcp, "mcp results of "+device.Name)})
		serverConn.Close()
	}()
	defer func() {
		cancel()
		clientConn.Close()
		<-served
	}()
	client := gomcp.NewClient(&gomcp.Implementation{Name: "sidecart3-s1", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(sessionCtx, &gomcp.IOTransport{Reader: clientConn, Writer: clientConn}, nil)
	if err != nil {
		return 0, err
	}
	defer clientSession.Close()
	tools, err := clientSession.ListTools(sessionCtx, nil)
	if err != nil {
		return 0, err
	}
	called := 0
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			continue
		}
		clientSession.CallTool(sessionCtx, &gomcp.CallToolParams{Name: tool.Name, Arguments: map[string]any{}})
		called += 1
	}
	return called, nil
}

// Server's log from here on: glog's files under dir at verbosity, added to net's capture as CC4's logs, each file
// streamed at each scan. Answers how many bytes server has logged so far, and what restores the flags it set.
func captureServerLogs(t testing.TB, capture *sidecartest.PrivacyCapture, dir string, verbosity string) (func() int64, func()) {
	t.Helper()
	restores := []func(){}
	for _, name := range []string{"logtostderr", "v"} {
		value := flag.Lookup(name).Value.String()
		restores = append(restores, func() { flag.Set(name, value) })
	}
	if err := glog.SetLogDir(dir); err != nil {
		t.Fatal(err)
	}
	flag.Set("logtostderr", "false")
	flag.Set("v", verbosity)
	files := func() []string {
		glog.Flush()
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		paths := []string{}
		for _, entry := range entries {
			if !entry.IsDir() {
				paths = append(paths, filepath.Join(dir, entry.Name()))
			}
		}
		return paths
	}
	capture.AddSource(func() []*sidecartest.PrivacyItem {
		items := []*sidecartest.PrivacyItem{}
		for _, path := range files() {
			items = append(items, &sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassLog, Name: "server's log " + filepath.Base(path), Open: func() (io.ReadCloser, error) {
				return os.Open(path)
			}})
		}
		return items
	})
	logged := func() int64 {
		total := int64(0)
		for _, path := range files() {
			if info, err := os.Stat(path); err == nil {
				total += info.Size()
			}
		}
		return total
	}
	return logged, func() {
		for _, restore := range restores {
			restore()
		}
	}
}

// S1's token clause: see the file header.
func TestScenarioS1TokenOnT3(t *testing.T) {
	logDir := t.TempDir()
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		RunScenario(t, &sidecartest.Scenario{Id: "S1", Run: func(ctx context.Context, run *sidecartest.ScenarioRun) error {
			serverLogged, restoreFlags := captureServerLogs(t, run.Net.Capture(), logDir, "2")
			defer restoreFlags()
			apiCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			network, err := newS1Network(apiCtx, t, run)
			if err != nil {
				return err
			}
			defer network.close()
			laptop, err := network.enroll(ctx, "laptop", nil)
			if err != nil {
				return err
			}
			bearer, err := network.bearer(laptop)
			if err != nil {
				return err
			}
			called, err := runS1McpSession(ctx, run, laptop)
			if err != nil {
				return fmt.Errorf("the MCP session: %w", err)
			}
			if called == 0 {
				return errors.New("the daemon's catalog served no read-only tool, so no MCP result was read")
			}
			// the token enrollment minted, presented where api refuses it: its canary, in the request's one user field, is
			// kept nowhere
			status, answer, err := network.api.Call(ctx, http.MethodPost, "/auth/code-create", bearer, map[string]any{
				"uses": 1, "duration_minutes": 5, "description": run.CanaryText("a code's description"),
			})
			if err != nil {
				return err
			}
			if status != http.StatusForbidden || strings.TrimSpace(string(answer)) != router.StrictExpCredentialRefusedMessage {
				return fmt.Errorf("code-create with an enrolled sidecar's token: %d %q", status, answer)
			}
			if serverLogged() == 0 {
				return errors.New("server logged nothing, so its log proves nothing")
			}
			return nil
		}}, nil)
	})
}

// S1's genesis clause: see the file header.
func TestScenarioS1GenesisOnT3(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		RunScenario(t, &sidecartest.Scenario{Id: "S1", Run: func(ctx context.Context, run *sidecartest.ScenarioRun) error {
			apiCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			network, err := newS1Network(apiCtx, t, run)
			if err != nil {
				return err
			}
			defer network.close()

			// the owner's first device signs the Genesis and base 1, which api's sequencer takes as entries 0 and 1
			owner, err := network.enroll(ctx, "owner laptop", nil)
			if err != nil {
				return err
			}
			ownerBearer, err := network.bearer(owner)
			if err != nil {
				return err
			}
			identity := owner.Sidecar().Identity()
			sign := func(purpose string, signedType bridge.SignedType, body []byte) (*bridge.Signed, error) {
				return owner.Sidecar().Sign(purpose, signedType, body, nil)
			}
			configRepoId := sidecar.NewId()
			heads := sidecartest.NewFakeConfigHeads(configRepoId)
			ownerRoutes := &t3ConfigRoutes{api: network.api, bearer: ownerBearer}
			bootstrap, err := sidecar.BootstrapNetwork(ctx, &sidecar.ConfigBootstrapArgs{
				NetworkId:         identity.NetworkId,
				OwnerMemberId:     sidecar.Id(network.userId),
				SidecarId:         identity.SidecarId,
				IdentityPublicKey: identity.IdentityPublicKey,
				Sign:              sign,
				ConfigRepoId:      configRepoId,
				Heads:             heads.ForSigner(identity.SidecarId, sign),
				Publisher:         ownerRoutes,
				Projection:        ownerRoutes,
				// policy text, which only the config repo holds: a comment compiles into no row api is sent
				Files: map[string][]byte{sidecar.ConfigHeadPolicyPath: []byte("// " + run.CanaryText("policy text") + "\n{}\n")},
				Clock: sidecar.RealClock{},
			})
			if err != nil {
				return fmt.Errorf("the owner's bootstrap through api: %w", err)
			}
			if !bootstrap.Won || bootstrap.Pin.GetVersion() != 1 {
				return fmt.Errorf("the owner's genesis did not take entry 1: won %t at %d", bootstrap.Won, bootstrap.Pin.GetVersion())
			}

			// a device's ConfigClient as its host builds one from the anchor its enrollment recorded, reading the log
			// with the device's own token
			follow := func(device *T3Sidecar) (*bridge.AuthPin, error) {
				anchor, err := device.Sidecar().Anchor()
				if err != nil {
					return nil, fmt.Errorf("%s recorded no anchor: %w", device.Name, err)
				}
				deviceBearer, err := network.bearer(device)
				if err != nil {
					return nil, err
				}
				settings := sidecar.DefaultConfigClientSettings()
				settings.Log = run.Net.Capture().Logger("config client of " + device.Name)
				client, err := sidecar.NewConfigClient(ctx, &sidecar.ConfigClientArgs{
					NetworkId:    identity.NetworkId,
					GenesisChain: [sha256.Size]byte(anchor.GenesisSha256),
					Projection:   &t3ConfigRoutes{api: network.api, bearer: deviceBearer},
					Heads:        heads,
					Clock:        sidecar.RealClock{},
					Settings:     settings,
				})
				if err != nil {
					return nil, err
				}
				defer client.Close()
				if err := client.CatchUp(ctx); err != nil {
					return nil, err
				}
				pin := client.Pin()
				if err := anchor.Check(anchor.GenesisSha256, pin); err != nil {
					return nil, err
				}
				return pin, nil
			}

			// an invite naming a genesis this network never had, as a server serving another log would present
			otherGenesis, err := sign(bridge.SignedPurposeAuthority, bridge.SignedType_SIGNED_GENESIS, func() []byte {
				body, _ := proto.Marshal(&bridge.Genesis{NetworkId: sidecar.NewId().Bytes(), ConfigRepoId: sidecar.NewId().Bytes(), OwnerMemberId: sidecar.Id(network.userId).Bytes(), OwnerSidecarId: identity.SidecarId.Bytes(), OwnerIdentityPublicKey: identity.IdentityPublicKey})
				return body
			}())
			if err != nil {
				return err
			}
			otherBytes, err := proto.Marshal(otherGenesis)
			if err != nil {
				return err
			}
			otherChain := bridge.GenesisChain(otherBytes)
			misled, err := network.enroll(ctx, "misled laptop", &sidecar.SidecarEnrollAnchor{GenesisSha256: otherChain[:]})
			if err != nil {
				return err
			}
			if _, err := follow(misled); !errors.Is(err, sidecar.ErrConfigClientGenesis) {
				return fmt.Errorf("a device invited to another genesis followed this network's log: %v", err)
			}

			// the twin: an invite naming the network's genesis and base 1, followed to base 1
			anchored, err := network.enroll(ctx, "invited laptop", &sidecar.SidecarEnrollAnchor{
				GenesisSha256: bootstrap.GenesisChain[:],
				HeadVersion:   bootstrap.Pin.GetVersion(),
				HeadChain:     bootstrap.Pin.GetChain(),
			})
			if err != nil {
				return err
			}
			pin, err := follow(anchored)
			if err != nil {
				return fmt.Errorf("a device invited to this network's genesis did not follow its log: %w", err)
			}
			if pin.GetVersion() != bootstrap.Pin.GetVersion() || !bytes.Equal(pin.GetChain(), bootstrap.Pin.GetChain()) {
				return fmt.Errorf("the invited device stands at %d, not base 1", pin.GetVersion())
			}
			return nil
		}}, nil)
	})
}

// S1's token clause has teeth: the same enrollment, with the token put where the clause says it must never be, as a
// bug would put it (server's log, an MCP result, the process environment), fails the scenario in each of those
// places, so the clause's pass is a scan that looked for the token and found nothing, never one that looked for none.
func TestScenarioS1TokenLeakIsCaughtOnT3(t *testing.T) {
	logDir := t.TempDir()
	var report *sidecartest.ScenarioReport
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		report = sidecartest.RunScenarioReport(t, &sidecartest.Scenario{Id: "S1", Run: func(ctx context.Context, run *sidecartest.ScenarioRun) error {
			_, restoreFlags := captureServerLogs(t, run.Net.Capture(), logDir, "2")
			defer restoreFlags()
			apiCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			network, err := newS1Network(apiCtx, t, run)
			if err != nil {
				return err
			}
			defer network.close()
			laptop, err := network.enroll(ctx, "leaky laptop", nil)
			if err != nil {
				return err
			}
			bearer, err := network.bearer(laptop)
			if err != nil {
				return err
			}
			token := strings.TrimPrefix(bearer, "Bearer ")
			glog.Infof("[s1]a token logged as a bug would log it: %s", token)
			fmt.Fprintf(run.Net.Capture().Writer(sidecartest.PrivacyClassMcp, "a leaking mcp result"), `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":%q}]}}`+"\n", token)
			// read at the scan, after the flow returns; the test's end restores it
			t.Setenv("SIDECART3_S1_LEAKED", token)
			run.CanaryText("a planted canary")
			return nil
		}}, ScenarioSettings(t, nil))
	})
	if report == nil {
		t.Fatal("the scenario did not run")
	}
	if report.FlowErr != nil {
		t.Fatalf("the flow: %v", report.FlowErr)
	}
	for _, class := range []sidecartest.PrivacyClass{sidecartest.PrivacyClassLog, sidecartest.PrivacyClassMcp, sidecartest.PrivacyClassEnv} {
		caught := slices.ContainsFunc(report.Findings, func(finding *sidecartest.PrivacyFinding) bool {
			return finding.Class == class && (strings.Contains(finding.Needle, "by_client_jwt") || strings.Contains(finding.Needle, "bearer"))
		})
		if !caught {
			t.Errorf("the token leaked into %s was not caught: %v", class, report.Findings)
		}
	}
}
