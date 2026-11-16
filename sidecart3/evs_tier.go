package sidecart3

// The evs tier of every T3 Net (EVS1-M2-40; sidecartest/fault's evs worlds): each evs instance a world starts runs
// server/cli/evs's block, evshost.NewHost, over the Net's real stores with the world's seams, and the world's devices
// present service tokens server's oauth mints, which that block's Authenticator verifies as a non-issuer block's
// mounted jwks would. The svc- key is the test's own, made and installed at the first token a Net's world asks for,
// and removed when the Net closes, so a Net with no evs world changes nothing of oauth's. A token asked for once the
// Net has closed, by a goroutine of a run that ended early, is refused: oauth without the test's key would read
// vault's auth.yml, which a test host does not hold, and its once-loaded config would then fail every later run.
//
// An instance's storage format reaches its block as an operator's does (QD-854): the tier stands the block's config,
// evs.yml, in with the instance format the world starts the instance at, and evshost.NewHost reads it from there,
// the engine settings the tier hands it naming none. So a world's rolled-back instance is server's block deployed
// with instance_format: 1. EvsTierSettings.LoadJobSettings does the same for the world's taskworker runs, through
// taskworker's own loader, which this package does not import.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
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

// The blocks an evs tier runs, as BlockConfigRead names them.
const (
	EvsTierBlockEvs        = "evs"
	EvsTierBlockTaskworker = "taskworker"
)

// What a Net's evs tier runs with beyond its defaults.
type EvsTierSettings struct {
	// The engine settings taskworker's evs jobs run with under the block's config standing in when it is called:
	// taskworker.LoadEvsJobSettings, which a test names since this package does not import taskworker. nil: the
	// world's taskworker runs take fault's own settings, this code's defaults at the world's format.
	LoadJobSettings func() (*evsservice.EvsSettings, error)
	// Told each time a block took its instance format from its config: an evs instance's start, or a taskworker
	// run's settings. nil: nobody.
	BlockConfigRead func(block string, instanceFormat uint32)
}

// The block's config a tier stands in for an instance format.
func evsTierBlockConfig(instanceFormat uint32) string {
	return fmt.Sprintf("# sidecart3's evs tier\ninstance_format: %d\n", instanceFormat)
}

// One Net's evs tier, and what removes the key it installed. settings may be nil.
func newEvsTier(settings *EvsTierSettings) (*fault.EvsTier, func()) {
	if settings == nil {
		settings = &EvsTierSettings{}
	}
	blockConfigRead := func(block string, instanceFormat uint32) {
		if settings.BlockConfigRead != nil {
			settings.BlockConfigRead(block, instanceFormat)
		}
	}
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
		NewHost: func(ctx context.Context, host *sidecar.ServiceHost, evsSettings *evsservice.EvsSettings, deps *fault.EvsTierDeps) (fault.EvsTierHost, error) {
			// the world's format goes through the block's config alone: the settings handed on name none, which no
			// engine starts under
			instanceFormat := evsSettings.InstanceFormat
			blockSettings := *evsSettings
			blockSettings.InstanceFormat = 0
			hostSettings := evshost.DefaultHostSettings()
			hostSettings.Evs = &blockSettings
			hostSettings.Hdd = deps.Hdd
			var evsHost *evshost.Host
			var err error
			evshost.Testing_WithBlockConfig(evsTierBlockConfig(instanceFormat), func() {
				evsHost, err = evshost.NewHost(ctx, host, hostSettings, &evshost.HostDeps{
					AuthState:   deps.AuthState,
					GateViews:   deps.GateViews,
					ReceiptKeys: deps.ReceiptKeys,
				})
			})
			if err != nil {
				return nil, err
			}
			if evsHost.InstanceFormat() != instanceFormat || evsHost.Engine().Settings().InstanceFormat != instanceFormat {
				evsHost.Close()
				return nil, fmt.Errorf("the evs block runs as instance format %d under a config naming %d", evsHost.Engine().Settings().InstanceFormat, instanceFormat)
			}
			blockConfigRead(EvsTierBlockEvs, instanceFormat)
			return evsHost, nil
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
	if settings.LoadJobSettings != nil {
		tier.JobSettings = func(instanceFormat uint32) (*evsservice.EvsSettings, error) {
			var evsSettings *evsservice.EvsSettings
			var err error
			evshost.Testing_WithBlockConfig(evsTierBlockConfig(instanceFormat), func() {
				evsSettings, err = settings.LoadJobSettings()
			})
			if err != nil {
				return nil, err
			}
			if evsSettings == nil || evsSettings.InstanceFormat != instanceFormat {
				return nil, fmt.Errorf("taskworker's evs jobs do not run as instance format %d under a config naming it: %+v", instanceFormat, evsSettings)
			}
			blockConfigRead(EvsTierBlockTaskworker, instanceFormat)
			return evsSettings, nil
		}
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
