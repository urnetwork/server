// `sidecarctl certs renew <env> [--dry-run]` keeps an env's platform certificates from expiring
// with nobody renewing them, which stops being safe to do by hand at 45-day lifetimes (D-OPS-12).
// A job on the builder runs it (xops/builder/BUILD.md), because only the builder holds the
// credentials it writes the vault with. Users' certificates are §13's and never pass through here:
// the names it renews are services.yml's alone, read through warp's own loader, so that it covers
// the names `warpctl certs issue` covers.
//
// lego orders one certificate per registered domain, with one key covering every name of that
// domain, as `warpctl certs issue` does, so a domain is what renews: one name falling due renews
// them all. A run pulls the builder's vault, reads each name's newest certificate where the lb
// build finds it, orders what is due, and adds the result to all/tls.pending as one new version,
// where `warpctl certs issue` puts one. A certificate is due at two thirds of its life (D-OPS-12),
// or earlier when its CA's ARI window (RFC 9773) says so.
//
// Nothing here replaces a version. A version's key can also be a jwt key, which tokens verify
// against for as long as they live (server/session/by_jwt_keyring.go), so a run only ever adds one,
// and a version that already exists stops the run's orders instead of being written into.
//
// No version reaches an lb from here. Making one live is settings.swapVersion's: the version has to
// reach every lb host, and `sidecarctl certs swap` has to run there (W5). The owner decided on
// 2026-11-06 (QD-022) that every lb stays on versioned paths for now, so the default refuses, and a
// person finishes the renewal with xops/certs/renew.md. Once a swap answers, the version moves from
// all/tls.pending to all/tls, which `warpctl certs issue` allows only once every host holds it.
//
// A failure never leaves a name quiet. Every run ends by warning of each domain whose served
// certificate is past its renewal point, so a renewal that fails, or lands and goes no further,
// warns on every run from two thirds of the certificate's life, a third of its life before it
// expires. Any warning exits 1, and the job alerts on that alone.

package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-semver/semver"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/warp/services"
)

// The image each order runs, pinned to an exact v4 release whose cli takes certsRenewLegoArgs'
// flags. It is the release `warpctl certs issue` runs (warpctl's legoImage), whose flags these copy,
// so the two move together. D-OPS-12 names lego v5, whose cli rejects `--accept-tos` and so this
// argument list: moving to it changes these flags and warpctl's certs issue with them.
const certsRenewLegoImage = "goacme/lego:v4.35.2"

// What starts each warning a run prints. The builder job posts these lines and no other output, so
// a warning holds only names, versions and dates, never what a command printed.
const certsRenewWarningPrefix = "certs renew warning: "

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "certs renew",
		usage:   "<env> [--dry-run]",
		summary: "Renew the env's platform certificates past two thirds of their life, or inside their CA's ARI window, into the builder's vault. --dry-run prints what is due and changes nothing.",
		run:     runCertsRenew,
	})
}

// How one run decides and acts. Tests replace the clock, lego, git and the swap.
type certsRenewSettings struct {
	// Reads what is due and prints it, and changes nothing: no git, no lego, no file.
	dryRun bool
	// The share of a certificate's life at which it renews, whatever its CA says. D-OPS-12's two
	// thirds leaves a third of the life to notice and fix a renewal that fails.
	renewLifeFraction float64
	// The CA's ACME directory: lego orders from it (--server), and ARI is found in it. Let's
	// Encrypt's, which `warpctl certs issue` orders from as lego's default.
	acmeDirectoryUrl string
	// Bounds each ARI request, so that a CA that hangs cannot hold up a renewal. ARI is advice, and
	// a request that fails falls back to renewLifeFraction. Non-positive expires every request at
	// once.
	ariRequestTimeout time.Duration
	// Bounds what one ARI response may read, in bytes. Both answers are small.
	ariResponseMaxByteCount int64
	// The time between runs of the builder job. A chosen renewal time before the next run renews
	// now (RFC 9773 section 4.2), so this has to be the job's own schedule: 6 hours, the RFC's
	// suggestion for checking, which leaves any window of a day or more at least one run inside it.
	runInterval time.Duration
	// How many times an order runs before its domain counts as failed. Each failed run can spend a
	// failed authorization, which a CA limits per hour, so this stays low. Non-positive orders
	// nothing, so every due domain fails.
	orderAttemptCount int
	// Bounds one lego run, which may wait route53PropagationTimeout for one record. A run past it
	// is killed and counts as a failed attempt. Non-positive kills every run at once.
	orderTimeout time.Duration
	// How long a lego run its timeout killed may hold its output open before the order stops
	// waiting for it. Non-positive waits until it closes.
	orderWaitDelay time.Duration
	// The key lego makes for an order, ec256 as `warpctl certs issue` makes.
	keyType string
	// How long lego waits for a route53 change to reach INSYNC, long enough for the slowest change
	// seen. lego reads whole seconds.
	route53PropagationTimeout time.Duration
	// The wait between lego's looks at a route53 change. lego reads whole seconds.
	route53PollingInterval time.Duration
	// How many times the aws client retries one route53 call.
	route53MaxRetryCount int
	// A flat wait after writing a cloudflare record, in place of a propagation check: cloudflare's
	// api answers before its anycast edge converges, and a check can validate one attempt behind.
	cloudflarePropagationWait time.Duration
	// The resolver lego checks records through, as host:port. The docker-internal resolver can
	// silently drop TXT queries and stall propagation checks.
	dnsResolver string
	// The time every certificate is judged at, and the date that names a new version.
	now func() time.Time
	// The home whose .aws and .cloudflare/credentials an order reads. Empty is this user's own.
	userHome string
	// Where lego's output and git's go: the job's log, never a warning, since either can quote a
	// credential or a remote's url.
	log io.Writer
	// Runs one lego order as `docker run` with these arguments after `run`, under ctx.
	runLego func(ctx context.Context, args []string) error
	// Runs one git command in the vault checkout at vaultHome, its arguments after
	// `git -C <vaultHome>`. What git prints goes to log, and the error says only which command
	// failed and how.
	git func(vaultHome string, args []string) error
	// Makes a committed version live on every lb host of env, and returns nil only once it is: the
	// version has reached each host, and `sidecarctl certs swap` has run there.
	swapVersion func(env string, version string) error
}

// The settings of a run on the builder. A caller changes its own copy.
func defaultCertsRenewSettings() *certsRenewSettings {
	settings := &certsRenewSettings{
		dryRun:                    false,
		renewLifeFraction:         2.0 / 3.0,
		acmeDirectoryUrl:          "https://acme-v02.api.letsencrypt.org/directory",
		ariRequestTimeout:         30 * time.Second,
		ariResponseMaxByteCount:   64 * 1024,
		runInterval:               6 * time.Hour,
		orderAttemptCount:         4,
		orderTimeout:              30 * time.Minute,
		orderWaitDelay:            10 * time.Second,
		keyType:                   "ec256",
		route53PropagationTimeout: 600 * time.Second,
		route53PollingInterval:    30 * time.Second,
		route53MaxRetryCount:      8,
		cloudflarePropagationWait: 20 * time.Second,
		dnsResolver:               "1.1.1.1:53",
		now:                       time.Now,
		userHome:                  "",
		log:                       os.Stderr,
	}
	settings.runLego = func(ctx context.Context, args []string) error {
		return runDockerLego(ctx, settings.orderWaitDelay, settings.log, args)
	}
	settings.git = func(vaultHome string, args []string) error {
		cmd := exec.Command("git", append([]string{"-C", vaultHome}, args...)...)
		cmd.Stdout = settings.log
		cmd.Stderr = settings.log
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil
	}
	settings.swapVersion = func(env string, version string) error {
		return fmt.Errorf(
			"version %s waits in all/tls.pending, and nothing makes it live from here: every lb stays on versioned paths for now (QD-022), so finish it by hand with xops/certs/renew.md",
			version,
		)
	}
	return settings
}

// One registered domain and the names one order covers.
type certsRenewDomain struct {
	domain string
	// how lego answers the domain's dns challenge, from services.yml's `domains`
	registrar string
	// in services.yml's order, the first naming the files lego writes
	hostnames []string
}

// An env's names by registered domain, in domain order, from the vault's services.yml as warpctl
// loads it. A name's domain is its last two labels, as `warpctl certs issue` takes it.
func loadCertsRenewDomains(vaultHome string, env string) ([]*certsRenewDomain, error) {
	servicesConfig, err := services.LoadServicesConfigFrom(vaultHome, env)
	if err != nil {
		return nil, err
	}
	domainRegistrars := servicesConfig.DomainRegistrars()

	domainHostnames := map[string][]string{}
	for _, host := range servicesConfig.Hostnames(env, []string{}) {
		domain := host
		if hostParts := strings.Split(host, "."); 2 < len(hostParts) {
			domain = strings.Join(hostParts[len(hostParts)-2:], ".")
		}
		if !slices.Contains(domainHostnames[domain], host) {
			domainHostnames[domain] = append(domainHostnames[domain], host)
		}
	}

	domains := []*certsRenewDomain{}
	for _, domain := range slices.Sorted(maps.Keys(domainHostnames)) {
		domains = append(domains, &certsRenewDomain{
			domain:    domain,
			registrar: domainRegistrars[domain],
			hostnames: domainHostnames[domain],
		})
	}
	return domains, nil
}

// The directory and file stem of a host's pair in a tls version, where star.<domain> is a
// wildcard's.
func certsRenewCertName(host string) string {
	if strings.HasPrefix(host, "*.") {
		return fmt.Sprintf("star.%s", host[len("*."):])
	}
	return host
}

// The arguments of `docker run` for one domain's order, from docker's own flags to lego's `run`.
// These must be what certsRenewLegoImage's cli takes, and are `warpctl certs issue`'s, with the CA
// named so that orders and ARI read one directory. envFilePath holds a cloudflare token, because a
// token in an argument would reach ps and every log that prints a command line.
func certsRenewLegoArgs(domain *certsRenewDomain, legoHome string, userHome string, envFilePath string, settings *certsRenewSettings) ([]string, error) {
	// `docker run` keeps a stopped container, and the environment in it
	args := []string{"--rm"}
	// the registrar's lego flags, which follow the image
	registrarLegoArgs := []string{}
	switch domain.registrar {
	case "route53":
		args = append(args,
			// see https://go-acme.github.io/lego/dns/route53/
			"-e", fmt.Sprintf("AWS_PROPAGATION_TIMEOUT=%d", int(settings.route53PropagationTimeout/time.Second)),
			"-e", fmt.Sprintf("AWS_POLLING_INTERVAL=%d", int(settings.route53PollingInterval/time.Second)),
			"-e", fmt.Sprintf("AWS_MAX_RETRIES=%d", settings.route53MaxRetryCount),
			"-v", fmt.Sprintf("%s:/root/.aws:z", filepath.Join(userHome, ".aws")),
		)
		// route53 gates on INSYNC, so the authoritative recheck is skipped
		registrarLegoArgs = append(registrarLegoArgs, "--dns.propagation-disable-ans")
	case "cloudflare":
		// see https://go-acme.github.io/lego/dns/cloudflare/
		args = append(args, "--env-file", envFilePath)
		registrarLegoArgs = append(registrarLegoArgs, "--dns.propagation-wait", settings.cloudflarePropagationWait.String())
	default:
		return nil, fmt.Errorf("services.yml gives the domain registrar %q, and only route53 and cloudflare can answer an order", domain.registrar)
	}
	args = append(args,
		"-v", fmt.Sprintf("%s:/.lego:Z", legoHome),
		certsRenewLegoImage,
		"--accept-tos",
		"--server", settings.acmeDirectoryUrl,
		"--key-type", settings.keyType,
		"--dns", domain.registrar,
		"--email", fmt.Sprintf("admin@%s", domain.domain),
		"--dns.resolvers", settings.dnsResolver,
	)
	args = append(args, registrarLegoArgs...)
	for _, host := range domain.hostnames {
		args = append(args, "--domains", host)
	}
	return append(args, "run"), nil
}

// Orders one domain's certificate into legoHome, running lego up to orderAttemptCount times.
func orderCertsRenewDomain(domain *certsRenewDomain, legoHome string, settings *certsRenewSettings) error {
	userHome := settings.userHome
	if userHome == "" {
		var err error
		if userHome, err = os.UserHomeDir(); err != nil {
			return err
		}
	}

	envFilePath := ""
	if domain.registrar == "cloudflare" {
		credentialsPath := filepath.Join(userHome, ".cloudflare", "credentials")
		credentialsBytes, err := os.ReadFile(credentialsPath)
		if err != nil {
			return fmt.Errorf("read the cloudflare credentials: %w", err)
		}
		credentials := struct {
			DnsApiTokens map[string]string `yaml:"cloudflare_dns_api_tokens"`
		}{}
		// a parse error can quote the file, which holds every domain's token
		if err := yaml.Unmarshal(credentialsBytes, &credentials); err != nil {
			return fmt.Errorf("%s does not parse as the cloudflare credentials", credentialsPath)
		}
		token := credentials.DnsApiTokens[domain.domain]
		if token == "" {
			return fmt.Errorf("%s has no cloudflare_dns_api_tokens entry for %s", credentialsPath, domain.domain)
		}
		// outside the vault, whose checkout a crash would leave it in, and readable by its owner alone
		envHome, err := os.MkdirTemp("", "certs-renew-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(envHome)
		envFilePath = filepath.Join(envHome, "cloudflare.env")
		if err := os.WriteFile(envFilePath, []byte(fmt.Sprintf("CF_DNS_API_TOKEN=%s\n", token)), 0600); err != nil {
			return err
		}
	}

	args, err := certsRenewLegoArgs(domain, legoHome, userHome, envFilePath, settings)
	if err != nil {
		return err
	}
	attemptErrs := []error{}
	for attemptIndex := 0; attemptIndex < settings.orderAttemptCount; attemptIndex += 1 {
		err := func() error {
			ctx, cancel := context.WithTimeout(context.Background(), settings.orderTimeout)
			defer cancel()
			return settings.runLego(ctx, args)
		}()
		if err == nil {
			return nil
		}
		attemptErrs = append(attemptErrs, fmt.Errorf("attempt %d: %w", attemptIndex+1, err))
	}
	if len(attemptErrs) == 0 {
		return errors.New("no attempt ran")
	}
	return errors.Join(attemptErrs...)
}

// Runs lego through the docker cli as warpctl runs docker, through sudo on linux, with its output
// in log, in a process group of its own so that ctx's end kills sudo's child too. The container a
// killed cli started can outlive it, until lego's own timeouts end it.
func runDockerLego(ctx context.Context, waitDelay time.Duration, log io.Writer, args []string) error {
	name := "docker"
	commandArgs := append([]string{"run"}, args...)
	if runtime.GOOS == "linux" {
		name = "sudo"
		commandArgs = append([]string{"docker"}, commandArgs...)
	}
	cmd := exec.CommandContext(ctx, name, commandArgs...)
	// lego's output is all there is to say why an order failed
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = waitDelay
	return cmd.Run()
}

// What one order wrote: one certificate, its issuer, one key, for every name of the domain.
type certsRenewLegoCertificate struct {
	crtBytes []byte
	caBytes  []byte
	keyBytes []byte
}

// Reads what an order wrote. lego names the files for the order's first name, with a wildcard's
// star as an underscore.
func readCertsRenewLegoCertificate(legoHome string, domain *certsRenewDomain) (*certsRenewLegoCertificate, error) {
	fileStem := strings.ReplaceAll(domain.hostnames[0], "*", "_")
	fileNameBytes := map[string][]byte{}
	for _, fileName := range []string{fileStem + ".crt", fileStem + ".issuer.crt", fileStem + ".key"} {
		fileBytes, err := os.ReadFile(filepath.Join(legoHome, "certificates", fileName))
		if err != nil {
			return nil, err
		}
		fileNameBytes[fileName] = fileBytes
	}
	return &certsRenewLegoCertificate{
		crtBytes: fileNameBytes[fileStem+".crt"],
		caBytes:  fileNameBytes[fileStem+".issuer.crt"],
		keyBytes: fileNameBytes[fileStem+".key"],
	}, nil
}

// The certificate and its chain, as an lb reads them. lego's crt usually holds the chain already.
func (self *certsRenewLegoCertificate) pemBytes() []byte {
	pemBytes := slices.Clone(self.crtBytes)
	if !strings.Contains(string(self.crtBytes), strings.TrimSpace(string(self.caBytes))) {
		pemBytes = append(pemBytes, '\n')
		pemBytes = append(pemBytes, self.caBytes...)
	}
	return pemBytes
}

// Refuses an order that could not serve: the key must match, and the certificate must be valid at
// now and cover every name. lego's exit of 0 promises none of that, and what passes here goes into
// the vault with nobody looking.
func (self *certsRenewLegoCertificate) verify(domain *certsRenewDomain, now time.Time) error {
	pair, err := tls.X509KeyPair(self.pemBytes(), self.keyBytes)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return fmt.Errorf("the certificate is valid from %s to %s", leaf.NotBefore, leaf.NotAfter)
	}
	for _, host := range domain.hostnames {
		// star.<domain> matches a wildcard, where the wildcard itself is no hostname to verify
		if err := leaf.VerifyHostname(certsRenewCertName(host)); err != nil {
			return err
		}
	}
	return nil
}

// Writes one directory per name under a tls version, as `warpctl certs issue` files them.
func (self *certsRenewLegoCertificate) writeNames(versionHome string, domain *certsRenewDomain) error {
	for _, host := range domain.hostnames {
		certName := certsRenewCertName(host)
		certHome := filepath.Join(versionHome, certName)
		if err := os.MkdirAll(certHome, 0700); err != nil {
			return err
		}
		fileNameBytes := map[string][]byte{
			certName + ".crt": self.crtBytes,
			"ca.crt":          self.caBytes,
			certName + ".key": self.keyBytes,
			certName + ".pem": self.pemBytes(),
		}
		for _, fileName := range slices.Sorted(maps.Keys(fileNameBytes)) {
			if err := os.WriteFile(filepath.Join(certHome, fileName), fileNameBytes[fileName], 0600); err != nil {
				return err
			}
		}
	}
	return nil
}

// A window a CA suggests renewing a certificate in (RFC 9773 section 4.2).
type ariWindow struct {
	start time.Time
	end   time.Time
}

// A time in the window. The RFC asks for a uniformly random one, which spreads a CA's load, kept
// once chosen. Hashing the certificate's ARI id picks the same time on every run, with nothing to
// store between runs.
func (self *ariWindow) chooseTime(ariId string) time.Time {
	digest := sha256.Sum256([]byte(ariId))
	offset := binary.BigEndian.Uint64(digest[:8]) % uint64(self.end.Sub(self.start))
	return self.start.Add(time.Duration(offset))
}

// A certificate's ARI id (RFC 9773 section 4.1): the key identifier of its Authority Key
// Identifier, a period, and its serial as DER content octets, each base64url without padding.
// big.Int drops the zero octet DER keeps in front of a serial whose high bit is set.
func ariCertificateId(leaf *x509.Certificate) (string, error) {
	if len(leaf.AuthorityKeyId) == 0 {
		return "", errors.New("the certificate has no authority key identifier")
	}
	serialBytes := leaf.SerialNumber.Bytes()
	if len(serialBytes) == 0 || serialBytes[0]&0x80 != 0 {
		serialBytes = append([]byte{0}, serialBytes...)
	}
	return fmt.Sprintf(
		"%s.%s",
		base64.RawURLEncoding.EncodeToString(leaf.AuthorityKeyId),
		base64.RawURLEncoding.EncodeToString(serialBytes),
	), nil
}

// Reads a CA's windows for one run. ARI is advice, so anything short of a good answer is logged and
// read as no window, and a certificate falls back to its own schedule. A run polls on its job's
// schedule and keeps no state, so a Retry-After is not read.
type ariReader struct {
	settings *certsRenewSettings
	// empty when the CA offers no ARI, or its directory did not read
	renewalInfoUrl string
	directoryRead  bool
	// what the CA answered for each id this run, nil for no window: a domain's names share one
	// certificate
	idWindows map[string]*ariWindow
}

// A reader that has asked the CA nothing yet.
func newAriReader(settings *certsRenewSettings) *ariReader {
	return &ariReader{
		settings:  settings,
		idWindows: map[string]*ariWindow{},
	}
}

// Decodes one small json document, bounded in time and in size.
func (self *ariReader) getJson(url string, value any) error {
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.ariRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", url, response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, self.settings.ariResponseMaxByteCount)).Decode(value)
}

// The certificate's window and ARI id, or a nil window when its CA offers none.
func (self *ariReader) window(leaf *x509.Certificate) (*ariWindow, string) {
	if !self.directoryRead {
		self.directoryRead = true
		directory := struct {
			RenewalInfo string `json:"renewalInfo"`
		}{}
		if err := self.getJson(self.settings.acmeDirectoryUrl, &directory); err != nil {
			fmt.Fprintf(self.settings.log, "Reading the ACME directory for ARI: %s\n", err)
		}
		self.renewalInfoUrl = directory.RenewalInfo
	}
	if self.renewalInfoUrl == "" {
		return nil, ""
	}

	ariId, err := ariCertificateId(leaf)
	if err != nil {
		fmt.Fprintf(self.settings.log, "No ARI for %s: %s\n", leaf.Subject, err)
		return nil, ""
	}
	if window, ok := self.idWindows[ariId]; ok {
		return window, ariId
	}

	renewalInfo := struct {
		SuggestedWindow struct {
			Start time.Time `json:"start"`
			End   time.Time `json:"end"`
		} `json:"suggestedWindow"`
	}{}
	var window *ariWindow
	url := fmt.Sprintf("%s/%s", strings.TrimSuffix(self.renewalInfoUrl, "/"), ariId)
	if err := self.getJson(url, &renewalInfo); err != nil {
		fmt.Fprintf(self.settings.log, "Reading ARI for %s: %s\n", ariId, err)
	} else if !renewalInfo.SuggestedWindow.Start.Before(renewalInfo.SuggestedWindow.End) {
		fmt.Fprintf(self.settings.log, "Ignoring ARI for %s: its window does not start before it ends\n", ariId)
	} else {
		window = &ariWindow{
			start: renewalInfo.SuggestedWindow.Start,
			end:   renewalInfo.SuggestedWindow.End,
		}
	}
	self.idWindows[ariId] = window
	return window, ariId
}

// The newest certificate the vault holds for a name in one kind of home.
type certsRenewHeld struct {
	// where it sits, relative to the vault, for a person to find
	path string
	// the version it is part of
	version string
	// nil when its file does not read, with readErr saying why
	leaf    *x509.Certificate
	readErr error
}

// When the certificate's life passes fraction.
func (self *certsRenewHeld) renewPoint(fraction float64) time.Time {
	life := self.leaf.NotAfter.Sub(self.leaf.NotBefore)
	return self.leaf.NotBefore.Add(time.Duration(float64(life) * fraction))
}

// A name's newest certificate in the first of tlsHomes that holds it, which is the order the lb
// build looks in (warpctl's findLatestTls), or nil when none does. A version holds a name when it
// has both of the name's files. tlsHomes are relative to vaultHome.
func readCertsRenewHeld(vaultHome string, tlsHomes []string, name string) (*certsRenewHeld, error) {
	pemFileName := name + ".pem"
	keyFileName := name + ".key"
	// warpctl's semverCmpWithBuild, which orders versions for the lb build: the numbers, then the
	// build metadata, ignoring a prerelease
	compareVersions := func(a semver.Version, b semver.Version) int {
		switch {
		case a.Major == b.Major && a.Minor == b.Minor && a.Patch == b.Patch:
			return strings.Compare(a.Metadata, b.Metadata)
		case a.LessThan(b):
			return -1
		default:
			return 1
		}
	}
	for _, tlsHome := range tlsHomes {
		entries, err := os.ReadDir(filepath.Join(vaultHome, tlsHome))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		versionDirNames := map[semver.Version]string{}
		for _, entry := range entries {
			// tls/current is no version, and passes here too
			version, err := semver.NewVersion(entry.Name())
			if err != nil || !entry.IsDir() {
				continue
			}
			holdsName := true
			for _, fileName := range []string{pemFileName, keyFileName} {
				if _, err := os.Stat(filepath.Join(vaultHome, tlsHome, entry.Name(), name, fileName)); err != nil {
					holdsName = false
				}
			}
			if holdsName {
				versionDirNames[*version] = entry.Name()
			}
		}
		if len(versionDirNames) == 0 {
			continue
		}
		versions := slices.SortedStableFunc(maps.Keys(versionDirNames), compareVersions)
		versionDirName := versionDirNames[versions[len(versions)-1]]

		held := &certsRenewHeld{
			path:    filepath.Join(tlsHome, versionDirName, name, pemFileName),
			version: versionDirName,
		}
		pemBytes, err := os.ReadFile(filepath.Join(vaultHome, held.path))
		if err != nil {
			return nil, err
		}
		if block, _ := pem.Decode(pemBytes); block == nil || block.Type != "CERTIFICATE" {
			held.readErr = errors.New("it does not start with a certificate")
		} else {
			held.leaf, held.readErr = x509.ParseCertificate(block.Bytes)
		}
		return held, nil
	}
	return nil, nil
}

// `sidecarctl certs renew <env> [--dry-run]`, in the vault warpctl reads with this environment.
func runCertsRenew(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("certs renew", flag.ContinueOnError)
	dryRun := flags.Bool("dry-run", false, "print what is due and change nothing")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return sidecarctlUsageErrorf("want <env>, got %q", positionals)
	}
	vaultHome, err := findWarpVaultHome()
	if err != nil {
		return err
	}
	settings := defaultCertsRenewSettings()
	settings.dryRun = *dryRun
	return renewCertificates(vaultHome, positionals[0], settings, out)
}

// Renews every domain with a name past its renewal point, or inside the window its CA suggests,
// whichever comes first. So a window before two thirds pulls a renewal forward, which is how a CA
// asks for a certificate it means to revoke, and a window after two thirds holds nothing back: a
// renewal has to be tried by two thirds for a failure to warn with a third of the life left.
//
// All that renews in a run is one new version, committed and pushed, then handed to
// settings.swapVersion, and once that answers it moves to all/tls. A domain that fails costs only
// itself, and the others land. A vault that will not pull stops the run before it orders anything,
// since an old checkout would order again what another run renewed. Every failure is a warning, and
// a run with any warning returns an error.
func renewCertificates(vaultHome string, env string, settings *certsRenewSettings, out io.Writer) error {
	problems := []error{}
	warn := func(err error) {
		fmt.Fprintf(out, "%s%s\n", certsRenewWarningPrefix, err)
		problems = append(problems, err)
	}

	if !settings.dryRun {
		if err := settings.git(vaultHome, []string{"pull", "--ff-only"}); err != nil {
			warn(fmt.Errorf("the vault will not pull, so nothing was ordered: %w", err))
			return errors.Join(problems...)
		}
	}

	domains, err := loadCertsRenewDomains(vaultHome, env)
	if err != nil {
		warn(fmt.Errorf("the services config of %s does not load, so nothing was ordered: %w", env, err))
		return errors.Join(problems...)
	}

	// what an lb build reads, in its order, and where a version waits to reach every host
	servedHomes := []string{filepath.Join(env, "tls"), filepath.Join("all", "tls")}
	pendingHomes := []string{filepath.Join(env, "tls.pending"), filepath.Join("all", "tls.pending")}

	// a version that does not read counts for less than one that reads
	newestHeld := func(served *certsRenewHeld, pending *certsRenewHeld) *certsRenewHeld {
		switch {
		case pending == nil:
			return served
		case served == nil, served.leaf == nil:
			return pending
		case pending.leaf != nil && pending.leaf.NotAfter.After(served.leaf.NotAfter):
			return pending
		default:
			return served
		}
	}

	ari := newAriReader(settings)
	// why the certificate is due, or empty when it is not
	dueReason := func(held *certsRenewHeld, now time.Time) string {
		if held == nil {
			return "it has no certificate"
		}
		if held.readErr != nil {
			return fmt.Sprintf("%s does not read: %s", held.path, held.readErr)
		}
		if !now.Before(held.renewPoint(settings.renewLifeFraction)) {
			return fmt.Sprintf("%s is past its renewal point", held.path)
		}
		if window, ariId := ari.window(held.leaf); window != nil {
			// the next run may come after the chosen time, so this run takes it
			if window.chooseTime(ariId).Before(now.Add(settings.runInterval)) {
				return fmt.Sprintf("%s is in its CA's renewal window", held.path)
			}
		}
		return ""
	}

	now := settings.now()
	dueDomains := []*certsRenewDomain{}
	for _, domain := range domains {
		reason := ""
		var readErr error
		for _, host := range domain.hostnames {
			name := certsRenewCertName(host)
			var served, pending *certsRenewHeld
			if served, readErr = readCertsRenewHeld(vaultHome, servedHomes, name); readErr != nil {
				break
			}
			if pending, readErr = readCertsRenewHeld(vaultHome, pendingHomes, name); readErr != nil {
				break
			}
			// a version in tls.pending counts, or every run would order again until a person moved it
			if nameReason := dueReason(newestHeld(served, pending), now); nameReason != "" {
				reason = fmt.Sprintf("%s: %s", name, nameReason)
				break
			}
		}
		switch {
		case readErr != nil:
			fmt.Fprintf(settings.log, "[%s]reading the vault: %s\n", domain.domain, readErr)
			warn(fmt.Errorf("%s: the vault cannot be read, so it was not ordered", domain.domain))
		case reason != "":
			fmt.Fprintf(out, "[%s]due, %s\n", domain.domain, reason)
			dueDomains = append(dueDomains, domain)
		default:
			fmt.Fprintf(out, "[%s]not due\n", domain.domain)
		}
	}

	if settings.dryRun {
		return nil
	}

	if 0 < len(dueDomains) {
		year, month, day := now.Date()
		version := fmt.Sprintf("%d.%d.%d", year, month, day)

		// a version is written once. Another today leaves this run nowhere to go until tomorrow's
		// version, which costs a day of a third of a lifetime.
		versionTaken := false
		for _, tlsHome := range slices.Concat(servedHomes, pendingHomes) {
			if _, err := os.Lstat(filepath.Join(vaultHome, tlsHome, version)); !errors.Is(err, os.ErrNotExist) {
				versionTaken = true
			}
		}
		switch {
		case versionTaken:
			warn(fmt.Errorf("version %s already exists, so nothing was ordered: the next day's run orders what is due", version))
		default:
			if err := renewCertsRenewDomains(vaultHome, version, dueDomains, settings, out, warn); err != nil {
				warn(err)
			} else if err := settings.swapVersion(env, version); err != nil {
				warn(err)
			} else {
				pendingPath := filepath.Join("all", "tls.pending", version)
				servedPath := filepath.Join("all", "tls", version)
				err := os.MkdirAll(filepath.Join(vaultHome, "all", "tls"), 0755)
				if err == nil {
					err = settings.git(vaultHome, []string{"mv", pendingPath, servedPath})
				}
				if err == nil {
					err = settings.git(vaultHome, []string{"commit", "-m", fmt.Sprintf("Move platform certificates %s out of tls.pending now every lb serves them", version), "--", pendingPath, servedPath})
				}
				if err == nil {
					err = settings.git(vaultHome, []string{"push"})
				}
				if err != nil {
					warn(fmt.Errorf("version %s is live but did not move out of tls.pending: %w", version, err))
				} else {
					fmt.Fprintf(out, "Moved %s to %s\n", pendingPath, servedPath)
				}
			}
		}
	}

	// read again, since what counts is where this run left each name
	for _, domain := range domains {
		pastCount := 0
		var soonest *certsRenewHeld
		waitingVersion := ""
		readFailed := false
		for _, host := range domain.hostnames {
			name := certsRenewCertName(host)
			served, servedErr := readCertsRenewHeld(vaultHome, servedHomes, name)
			pending, pendingErr := readCertsRenewHeld(vaultHome, pendingHomes, name)
			if servedErr != nil || pendingErr != nil {
				readFailed = true
				break
			}
			if served != nil && served.leaf != nil && now.Before(served.renewPoint(settings.renewLifeFraction)) {
				continue
			}
			pastCount += 1
			if served != nil && served.leaf != nil && (soonest == nil || served.leaf.NotAfter.Before(soonest.leaf.NotAfter)) {
				soonest = served
			}
			if newest := newestHeld(served, pending); newest != nil && newest == pending && newest.leaf != nil && now.Before(newest.renewPoint(settings.renewLifeFraction)) {
				waitingVersion = pending.version
			}
		}
		if readFailed || pastCount == 0 {
			// a vault that does not read has warned above
			continue
		}
		state := "and nothing has replaced them"
		if waitingVersion != "" {
			state = fmt.Sprintf("and version %s waits in tls.pending", waitingVersion)
		}
		expiry := "with none serving"
		if soonest != nil {
			expiry = fmt.Sprintf(
				"the soonest expiring %s, in %d days,",
				soonest.leaf.NotAfter.UTC().Format(time.DateOnly),
				int(soonest.leaf.NotAfter.Sub(now).Hours()/24),
			)
		}
		warn(fmt.Errorf("%s: %d names are past their renewal point, %s %s", domain.domain, pastCount, expiry, state))
	}

	return errors.Join(problems...)
}

// Orders each due domain, and adds what succeeded to all/tls.pending as one version in one commit,
// pushed. A domain that fails warns and costs only itself. A step that fails after an order has
// nothing to fall back on, so it returns.
func renewCertsRenewDomains(
	vaultHome string,
	version string,
	dueDomains []*certsRenewDomain,
	settings *certsRenewSettings,
	out io.Writer,
	warn func(error),
) error {
	legoRoot := filepath.Join(vaultHome, ".lego")
	if err := os.MkdirAll(legoRoot, 0700); err != nil {
		return err
	}
	// holds an account key and every order's key until the run ends
	legoHome, err := os.MkdirTemp(legoRoot, "renew.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(legoHome)
	// a version appears in the vault whole or not at all, by one rename, so the stage is on the
	// vault's own filesystem
	stageHome := legoHome + ".stage"
	if err := os.Mkdir(stageHome, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(stageHome)

	renewedCount := 0
	for _, domain := range dueDomains {
		fmt.Fprintf(out, "[%s]renewing %s\n", domain.domain, strings.Join(domain.hostnames, ","))
		order := func() (*certsRenewLegoCertificate, error) {
			if err := orderCertsRenewDomain(domain, legoHome, settings); err != nil {
				return nil, err
			}
			certificate, err := readCertsRenewLegoCertificate(legoHome, domain)
			if err != nil {
				return nil, err
			}
			if err := certificate.verify(domain, settings.now()); err != nil {
				return nil, err
			}
			return certificate, nil
		}
		certificate, err := order()
		if err != nil {
			fmt.Fprintf(settings.log, "[%s]renewal failed: %s\n", domain.domain, err)
			warn(fmt.Errorf("%s: the renewal failed, and the job's log says why", domain.domain))
			continue
		}
		if err := certificate.writeNames(stageHome, domain); err != nil {
			return fmt.Errorf("%s: staging the renewal failed, so version %s was not added: %w", domain.domain, version, err)
		}
		renewedCount += 1
	}
	if renewedCount == 0 {
		return errors.New("no domain renewed, so there is no version to add")
	}

	pendingHome := filepath.Join(vaultHome, "all", "tls.pending")
	if err := os.MkdirAll(pendingHome, 0755); err != nil {
		return err
	}
	if err := os.Rename(stageHome, filepath.Join(pendingHome, version)); err != nil {
		return err
	}
	// the commit names the version alone, so nothing else in the checkout rides along
	pendingPath := filepath.Join("all", "tls.pending", version)
	if err := settings.git(vaultHome, []string{"add", "--", pendingPath}); err != nil {
		return fmt.Errorf("version %s is written but not committed: %w", version, err)
	}
	if err := settings.git(vaultHome, []string{"commit", "-m", fmt.Sprintf("Add renewed platform certificates %s", version), "--", pendingPath}); err != nil {
		return fmt.Errorf("version %s is written but not committed: %w", version, err)
	}
	if err := settings.git(vaultHome, []string{"push"}); err != nil {
		return fmt.Errorf("version %s is committed but not pushed: %w", version, err)
	}
	fmt.Fprintf(out, "Added %s\n", pendingPath)
	return nil
}
