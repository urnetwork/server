package work

// certs' install in taskworker (QD-846, question tls-m5-17-certs-host-install;
// §13.8, §19.4): what the order, renewal and revocation chains run on
// (SetTlsCertChains), behind sidecar.yml's certs switch. While the switch is
// off, absent or unreadable on a first read, nothing is built, no vault file is
// read, and each chain runs empty passes. While it is on, taskworker holds the
// engine, certsservice.CertsService on certs.yml's settings, over:
//
//   - the Postgres store, model.NewCertsStore;
//   - its DNS client, server.NewCertsDnsClient on certs.yml's dns_url with the
//     bearer of vault's internal-certs.yml, the only way certs writes DNS-01
//     (D-DNS-8). A certs.yml with no dns_url reads no bearer and leaves the
//     engine on certsservice.ClosedCertsDns, so it reaches no zone and every
//     order stops at its challenge;
//   - its ACME client, certsservice.NewCertsAcme on the account files certs.yml
//     names by each CA's account_file, read from vault. A CA certs.yml names no
//     account file for has no account, and the client refuses every order to
//     it;
//   - its hint writer, pushservice's on the host's Redis, which carries TlsHint
//     to the client that ordered.
//
// Every file is read once, when the switch turns on; a changed file is read
// again when the switch goes off and on, or the process restarts. A file that
// is missing or refused leaves certs uninstalled, said once in the log by the
// file's name and the part, never by anything the file holds: certs.yml, an
// account file it names, an account file that holds another CA's account, or
// internal-certs.yml while dns_url is set. Each step of the switch's install
// looks again, so a file that arrives installs certs with no restart.
//
// The canary verdict is as plan-TLS-canary-signal has it: the DNS client's
// CanaryClean is GET /dns/ready answering ready, so an order waits at DNS
// while the canary is not clean, and this install adds no other gate to it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// certs.yml under config/<env>.
const tlsCertChainsConfigFileName = "certs.yml"

// What the install is built from is missing or refused. The text names the
// file or the part, never a value read from one.
var ErrTlsCertChainsInstall = errors.New("certs' chains are not installed")

// Tunables of taskworker's certs install.
type TlsCertChainsInstallSettings struct {
	// How the install follows sidecar.yml's certs switch.
	Switch *server.SidecarSwitchInstallSettings
	// The DNS client's timeouts and bounds. Its origin and root are certs.yml's
	// dns_url and root, set at the build.
	DnsClient *server.CertsDnsClientSettings
}

func DefaultTlsCertChainsInstallSettings() *TlsCertChainsInstallSettings {
	return &TlsCertChainsInstallSettings{
		Switch:    server.DefaultSidecarSwitchInstallSettings(),
		DnsClient: server.DefaultCertsDnsClientSettings(),
	}
}

// What the install is built from. Each nil takes taskworker's own.
type TlsCertChainsInstallDeps struct {
	// Whether sidecar.yml's certs switch reads on. nil: this process's reader.
	SwitchOn func() bool
	// A file of config/<env> by name. nil: this process's config mount.
	ReadConfigFile func(name string) ([]byte, error)
	// A secret_file by name. nil: this process's vault mount.
	ReadSecretFile func(name string) ([]byte, error)
	// The host, built when the switch turns on: Redis for the budget and the
	// hints, and the clock. nil: sidecarhost.NewServiceHost on this process's
	// Redis.
	Host func(ctx context.Context) (*sidecar.ServiceHost, error)
	// The rows. nil: model.NewCertsStore on this process's Postgres, in a
	// binary that links MG5.
	Store certsservice.CertsStore
	// What the sweep reads. nil: the Postgres source.
	RevocationSource certsservice.CertsRevocationSource
	// How the dns api is reached. nil: http.DefaultTransport.
	DnsTransport http.RoundTripper
	// How every CA is reached. nil: a client on the system's roots.
	AcmeHttpClient *http.Client
	// What the CAA checks resolve with (TLS-M6-04). nil: a resolver on the
	// system's.
	CaaResolver certsservice.CertsCaaResolver
	// The custom hosts the CAA checks judge. nil: model.NewCertsCaaStore.
	CaaStore certsservice.CertsCaaStore
}

// taskworker's certs install as server.SidecarSwitchInstall drives it: nothing
// is built until a step finds the switch on. deps and settings may be nil.
func NewTlsCertChainsInstall(deps *TlsCertChainsInstallDeps, settings *TlsCertChainsInstallSettings) *server.SidecarSwitchInstall {
	if deps == nil {
		deps = &TlsCertChainsInstallDeps{}
	}
	if settings == nil {
		settings = DefaultTlsCertChainsInstallSettings()
	}
	switchOn := deps.SwitchOn
	if switchOn == nil {
		switchOn = func() bool {
			return server.CurrentSidecarConfig().Certs()
		}
	}
	return server.NewSidecarSwitchInstall("certs", switchOn, func(ctx context.Context) (func(), error) {
		return buildTlsCertChains(ctx, deps, settings)
	}, settings.Switch)
}

// Starts taskworker's certs install: the order, renewal and revocation chains
// run on the engine while sidecar.yml's certs switch is on and its files are
// there, and run empty passes otherwise. It follows the switch until ctx ends
// or the returned call, then removes what it installed; the call returns once
// it has.
func InstallTlsCertChains(ctx context.Context) (remove func()) {
	install := NewTlsCertChainsInstall(nil, nil)
	runCtx, cancel := context.WithCancel(ctx)
	var lifetime sync.WaitGroup
	lifetime.Go(func() {
		server.HandleError(func() {
			install.Run(runCtx)
		})
	})
	return func() {
		cancel()
		lifetime.Wait()
	}
}

// Builds the engine and installs it as the chains'; the returned call removes
// it. See the file header.
func buildTlsCertChains(ctx context.Context, deps *TlsCertChainsInstallDeps, settings *TlsCertChainsInstallSettings) (uninstall func(), err error) {
	refuse := func(format string, arguments ...any) (func(), error) {
		return nil, fmt.Errorf("%w: %s", ErrTlsCertChainsInstall, fmt.Sprintf(format, arguments...))
	}
	store := deps.Store
	if store == nil {
		if !server.SidecarMg5Linked() {
			return refuse("this binary links no tls tables (MG5)")
		}
		store = model.NewCertsStore(nil)
	}
	readConfigFile := deps.ReadConfigFile
	if readConfigFile == nil {
		readConfigFile = func(name string) ([]byte, error) {
			return server.ReadSidecarInstallFile(server.Config, "config", name)
		}
	}
	readSecretFile := deps.ReadSecretFile
	if readSecretFile == nil {
		readSecretFile = func(name string) ([]byte, error) {
			return server.ReadSidecarInstallFile(server.Vault, "vault", name)
		}
	}
	// a reader's error names its file; nothing a secret_file holds is passed on
	data, err := readConfigFile(tlsCertChainsConfigFileName)
	if err != nil {
		return refuse("%s", err)
	}
	config, err := server.ParseCertsConfig(data)
	if err != nil {
		return refuse("%s", err)
	}

	// the accounts, in CA order: every file certs.yml names must be there and hold that CA's account
	cas := make([]certsservice.CertsCa, 0, len(config.AccountFiles))
	for ca := range config.AccountFiles {
		cas = append(cas, ca)
	}
	slices.Sort(cas)
	accountFiles := make([][]byte, 0, len(cas))
	for _, ca := range cas {
		name := config.AccountFiles[ca]
		accountFile, err := readSecretFile(name)
		if err != nil {
			return refuse("CA %s's account file: %s", ca, err)
		}
		// checked alone first, so a refusal names its file
		account, err := certsservice.NewCertsAccountSet([][]byte{accountFile})
		if err != nil {
			// the engine's refusal names the field and never a value
			return refuse("vault's %s: %s", name, err)
		}
		if held := account.Cas(); len(held) != 1 || held[0] != ca {
			return refuse("vault's %s does not hold CA %s's account", name, ca)
		}
		accountFiles = append(accountFiles, accountFile)
	}
	accounts, err := certsservice.NewCertsAccountSet(accountFiles)
	if err != nil {
		return refuse("the account files: %s", err)
	}

	// the DNS client, only where certs.yml names the dns api
	var dns certsservice.CertsDns = certsservice.ClosedCertsDns{}
	var dnsClient *server.CertsDnsClient
	if config.DnsUrl != "" {
		credentialFile, err := readSecretFile(server.CertsDnsCredentialFileName)
		if err != nil {
			return refuse("the dns bearer: %s", err)
		}
		bearer, err := dnsservice.LoadDnsInternalCredential(credentialFile)
		if err != nil {
			// the cause may quote the file
			return refuse("vault's %s is not a credential file with a current bearer", server.CertsDnsCredentialFileName)
		}
		dnsSettings := server.DefaultCertsDnsClientSettings()
		if settings.DnsClient != nil {
			*dnsSettings = *settings.DnsClient
		}
		dnsSettings.BaseUrl = config.DnsUrl
		dnsSettings.Root = config.Settings.RootDomain
		client, err := server.NewCertsDnsClient(dnsSettings, bearer, deps.DnsTransport)
		if err != nil {
			// the client's refusal names the part, never the bearer
			return refuse("the dns client on %s's dns_url: %s", tlsCertChainsConfigFileName, err)
		}
		dns = client
		dnsClient = client
	} else {
		glog.Infof("[certs]%s names no dns_url, so certs writes no challenge and no order passes DNS\n", tlsCertChainsConfigFileName)
	}

	var host *sidecar.ServiceHost
	if deps.Host != nil {
		host, err = deps.Host(ctx)
		if err != nil {
			return refuse("the host: %s", err)
		}
	} else {
		host = sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{})
	}
	if host == nil || host.Redis == nil {
		return refuse("no host with Redis")
	}
	// a copy carrying certs' hint writer: a TlsHint goes by client id, so it needs no sidecar resolver
	certsHost := *host
	writer, err := pushservice.NewHintWriter(host.Redis, nil, nil, host.Metrics)
	if err != nil {
		return refuse("the hint writer: %s", err)
	}
	certsHost.Push = writer

	acme, err := certsservice.NewCertsAcme(config.Settings, &certsservice.CertsAcmeDeps{
		Accounts:   accounts,
		Dns:        dns,
		Clock:      host.Clock,
		HttpClient: deps.AcmeHttpClient,
	})
	if err != nil {
		return refuse("the ACME client: %s", err)
	}
	service, err := certsservice.NewCertsService(ctx, &certsHost, store, dns, acme, config.Settings)
	if err != nil {
		return refuse("the engine: %s", err)
	}
	installed := setTlsCertChains(&TlsCertChainDeps{Service: service, RevocationSource: deps.RevocationSource})
	// the custom-domain sweep on the same engine (TLS-M6-05), and the delegated DNS-01 challenges api hands over
	// (QD-1165), only where certs reaches the dns api, since their writes go through its client alone
	var installedSweep *TlsCustomSweepDeps
	var installedChallenges *controller.TlsCustomChallengeWorkerDeps
	if customStore, err := model.NewTlsCustomStore(&model.TlsCustomStoreOptions{Roots: config.CaaOperatorZones[:1]}); err == nil {
		installedSweep = SetTlsCustomSweep(&TlsCustomSweepDeps{Service: service, Store: customStore, Settings: certsservice.DefaultCertsCustomSettings()})
		if dnsClient != nil {
			installedChallenges = controller.SetTlsCustomChallengeWorker(&controller.TlsCustomChallengeWorkerDeps{Store: customStore, Dns: dnsClient})
		}
	} else {
		glog.Warningf("[certs]the custom sweep is not installed: %v\n", err)
	}
	// the CAA checks, only where certs reaches the canary route (TLS-M6-04)
	uninstallCaa := func() {}
	if dnsClient != nil {
		caaStore := deps.CaaStore
		if caaStore == nil {
			caaStore = model.NewCertsCaaStore(nil)
		}
		uninstallCaa = installTlsCaaChecks(&tlsCaaChecksBuild{
			service:       service,
			dns:           dnsClient,
			resolver:      deps.CaaResolver,
			store:         caaStore,
			operatorZones: config.CaaOperatorZones,
		})
	}
	return func() {
		// only this install's deps: a later install's stay
		tlsCertChainDeps.CompareAndSwap(installed, nil)
		tlsCustomSweepDeps.CompareAndSwap(installedSweep, nil)
		controller.UnsetTlsCustomChallengeWorker(installedChallenges)
		uninstallCaa()
	}, nil
}
