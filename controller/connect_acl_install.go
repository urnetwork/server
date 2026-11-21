package controller

// Connect follows each network through the projection reader appropriate to its
// process. Contract and peer authorization use the same verified state.

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/sidecar"
)

// Only an enabled contract gate polls the database for ACL heads.
func installContractGateWhenEnabled(ctx context.Context, cache *sidecar.AclGateCache) func() {
	install := server.NewSidecarSwitchInstall("contract-authorization", func() bool {
		return server.CurrentSidecarConfig().AclContractGate()
	}, func(ctx context.Context) (func(), error) {
		gate := NewAclContractGate(ctx, cache, nil)
		restore := SetAclContractGate(gate)
		return func() {
			restore()
			gate.Close()
		}, nil
	}, nil)
	return runAuthorizationInstall(ctx, install)
}

// Connect has no metadata credentials. Its cache verifies the API's bytes and
// receives version events on its own Redis subscriber.
func InstallConnectAuthorization(ctx context.Context, host *sidecar.ServiceHost) func() {
	install := server.NewSidecarSwitchInstall("connect-authorization", func() bool {
		return server.CurrentSidecarConfig().AclContractGate()
	}, func(ctx context.Context) (func(), error) {
		contractGateChanges.NotifyAll()
		if !server.SidecarMg2Linked() {
			return nil, errors.New("the ACL heads migration is unavailable")
		}
		apiUrl := os.Getenv(sidecarhost.ApiUrlEnvName)
		if apiUrl == "" {
			var err error
			apiUrl, err = session.DpopApiOrigin()
			if err != nil {
				return nil, err
			}
		}
		bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerConnect)
		if err != nil {
			return nil, errors.New("the connect projection credential is unavailable")
		}
		source, err := sidecar.NewAclGateApiSource(apiUrl, &http.Client{}, func() (string, error) { return bearer, nil }, nil)
		if err != nil {
			return nil, err
		}
		cache, closeCache, err := sidecarhost.WatchAclGate(ctx, host, source, nil)
		if err != nil {
			return nil, err
		}
		gate := NewAclContractGate(ctx, cache, nil)
		restore := SetAclContractGate(gate)
		return func() {
			restore()
			gate.Close()
			closeCache()
		}, nil
	}, nil)
	return runAuthorizationInstall(ctx, install)
}
