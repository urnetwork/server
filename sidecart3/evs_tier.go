package sidecart3

// The evs tier of every T3 Net (EVS1-M2-40; sidecartest/fault's evs worlds): each evs instance a world starts runs
// server/cli/evs's block, evshost.NewHost, over the Net's real stores with the world's seams, and the world's devices
// present service tokens server's oauth mints, which that block's Authenticator verifies as a non-issuer block's
// mounted jwks would. The svc- key is the test's own, made and installed at the first token a Net's world asks for,
// and removed when the Net closes, so a Net with no evs world changes nothing of oauth's. A token asked for once the
// Net has closed, by a goroutine of a run that ended early, is refused: oauth without the test's key would read
// vault's auth.yml, which a test host does not hold, and its once-loaded config would then fail every later run.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"sync"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/evshost"
	"github.com/urnetwork/server/oauth"
)

// The issuer the test's svc- key signs as: synthetic, and reachable by nothing.
const evsTierIssuer = "https://auth.example"

// A token asked of a Net's evs tier once the Net has closed.
var errEvsTierRemoved = errors.New("the evs tier's Net has closed, and its svc- key with it")

// One Net's evs tier, and what removes the key it installed.
func newEvsTier() (*fault.EvsTier, func()) {
	var installOnce sync.Once
	var installErr error
	stateLock := sync.RWMutex{}
	removed := false
	pops := []func(){}
	install := func() error {
		installOnce.Do(func() {
			privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				installErr = err
				return
			}
			thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
			if err != nil {
				installErr = err
				return
			}
			key := &oauth.SignerKey{
				Kid:        oauth.SignerKeySetService.KidPrefix() + thumbprint,
				Alg:        "ES256",
				Set:        oauth.SignerKeySetService,
				PrivateKey: privateKey,
			}
			stateLock.Lock()
			defer stateLock.Unlock()
			if removed {
				installErr = errEvsTierRemoved
				return
			}
			pops = append(pops, oauth.Testing_SetIssuer(evsTierIssuer), oauth.Testing_SetSignerKeys(key))
			verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
				Issuer: evsTierIssuer,
				Keys:   oauth.PublicJwks(oauth.SignerKeySetService).Keys,
			})
			if err != nil {
				installErr = err
				return
			}
			pops = append(pops, oauth.Testing_SetServiceTokenVerifier(verifier))
		})
		return installErr
	}
	tier := &fault.EvsTier{
		NewHost: func(ctx context.Context, host *sidecar.ServiceHost, settings *evsservice.EvsSettings, deps *fault.EvsTierDeps) (fault.EvsTierHost, error) {
			hostSettings := evshost.DefaultHostSettings()
			hostSettings.Evs = settings
			hostSettings.Hdd = deps.Hdd
			return evshost.NewHost(ctx, host, hostSettings, &evshost.HostDeps{
				AuthState:   deps.AuthState,
				GateViews:   deps.GateViews,
				ReceiptKeys: deps.ReceiptKeys,
			})
		},
		IssueToken: func(principal *sidecar.Principal) (string, error) {
			if err := install(); err != nil {
				return "", err
			}
			stateLock.RLock()
			defer stateLock.RUnlock()
			if removed {
				return "", errEvsTierRemoved
			}
			args := &oauth.MintServiceTokenArgs{
				NetworkId: server.Id(principal.NetworkId),
				ClientId:  server.Id(principal.ClientId),
				Audience:  principal.Audience,
				Scopes:    principal.Scopes,
			}
			if principal.SidecarId != nil {
				sidecarId := server.Id(*principal.SidecarId)
				args.SidecarId = &sidecarId
			}
			token, _, err := oauth.MintServiceToken(args)
			return token, err
		},
	}
	remove := func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		removed = true
		for index := len(pops) - 1; 0 <= index; index -= 1 {
			pops[index]()
		}
		pops = nil
	}
	return tier, remove
}
