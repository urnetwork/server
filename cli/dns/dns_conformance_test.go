//go:build sidecar_pending

package main

// §22.9's DNS row locally (DNS-M5-24; D-DNS-5, R-DNS-10): independent validators hold one edge's answers, on the
// private stack's zone, to DNSSEC with the root's KSK as their one trust anchor, as the anchored monitors do before the
// DS (§12.6): a signed answer, a wildcard answer signed as the queried name, and compact denial for a name that does
// not exist (RFC 9824) and for a type a name lacks, each validates; with another KSK as the anchor none does.
//
// The edge answers on 127.0.0.1 at ports above 50000 beside the harness's in-memory ones. BIND's delv runs from the
// host where it is installed. Unbound, Knot Resolver, BIND's named and zonemaster run in containers named
// sidecar-test-dns-<tool>, built by local/dns-conformance/build.sh, which fetches their packages: where an image is not
// on the host, its test says not run, which is no pass. The same tests run at T6 against test roots (External).

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/server"
)

// The lowest port the conformance tools bind on loopback: above every port the private stack and production's data
// tunnels use (BRIEF.md).
const dnsConformanceLowestPort = 50000

// How long one validator run may take: a bound for a failure.
const dnsConformanceToolTimeout = 30 * time.Second

// One question a validator must answer validated, and what its answer is.
type dnsConformanceQuestion struct {
	what   string
	name   string
	rrType uint16
	// the rcode a validated answer carries: compact denial answers a missing name NOERROR (RFC 9824)
	negative bool
}

// The edge on a real socket, the KSK as served, and the questions.
type dnsConformance struct {
	stack     *dnsT3
	port      int
	anchor    *dns.DNSKEY
	questions []dnsConformanceQuestion
}

// Listens on 127.0.0.1 for TCP and UDP at one port above dnsConformanceLowestPort that both take.
func dnsConformanceListen(t testing.TB) (net.Listener, net.PacketConn, int) {
	t.Helper()
	for range 64 {
		port := dnsConformanceLowestPort + 1 + rand.IntN(9999)
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		listener, err := net.Listen("tcp", address)
		if err != nil {
			continue
		}
		packetConn, err := net.ListenPacket("udp", address)
		if err != nil {
			listener.Close()
			continue
		}
		return listener, packetConn, port
	}
	t.Fatal("no port above 50000 took both TCP and UDP")
	return nil, nil, 0
}

// The T3 stack with a network, its first edge also serving 127.0.0.1 at a port of its own.
func newDnsConformance(t testing.TB, ctx context.Context) *dnsConformance {
	t.Helper()
	stack := newDnsT3(t, ctx, nil)
	_, apex := stack.network(t, ctx, "")
	stack.awaitLatest(t, ctx)
	edge := stack.harness.Edge(stack.settings.Ns[0])
	listener, packetConn, port := dnsConformanceListen(t)
	served := make(chan struct{}, 2)
	go func() {
		defer func() { served <- struct{}{} }()
		edge.Serve(listener)
	}()
	go func() {
		defer func() { served <- struct{}{} }()
		edge.ServePacket(packetConn)
	}()
	t.Cleanup(func() {
		listener.Close()
		packetConn.Close()
		<-served
		<-served
	})
	self := &dnsConformance{stack: stack, port: port}
	reply := stack.query(t, ctx, stack.settings.Ns[0], stack.root, dns.TypeDNSKEY, false)
	for _, record := range reply.Answer {
		if key, ok := record.(*dns.DNSKEY); ok && key.Flags&dns.SEP != 0 {
			self.anchor = key
		}
	}
	if self.anchor == nil {
		t.Fatalf("no KSK served: %v", reply)
	}
	self.questions = []dnsConformanceQuestion{
		{what: "a signed answer", name: apex, rrType: dns.TypeMX},
		{what: "the root's own record", name: "_spf." + stack.root, rrType: dns.TypeTXT},
		{what: "a wildcard answer", name: "www." + apex, rrType: dns.TypeA},
		{what: "compact denial of a missing name", name: "nobody." + stack.root, rrType: dns.TypeA, negative: true},
		// _dmarc answers *'s A, AAAA and MX through its copy (QD-1190), and holds no SRV
		{what: "compact denial of a missing type", name: "_dmarc." + apex, rrType: dns.TypeSRV, negative: true},
	}
	return self
}

// The anchor as BIND's trusted-keys holds it.
func dnsConformanceTrustedKeys(key *dns.DNSKEY) string {
	return fmt.Sprintf("trusted-keys {\n  %q %d %d %d %q;\n};\n", key.Hdr.Name, key.Flags, key.Protocol, key.Algorithm, key.PublicKey)
}

// Another KSK, which signs nothing here: a validator anchored to it must validate nothing.
func dnsConformanceOtherAnchor(t testing.TB, root string) *dns.DNSKEY {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: root, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	if _, err := key.Generate(256); err != nil {
		t.Fatal(err)
	}
	return key
}

// What delv says of question, anchored to key: its output.
func (self *dnsConformance) delv(t testing.TB, delvPath string, key *dns.DNSKEY, question dnsConformanceQuestion) string {
	t.Helper()
	anchorFile := filepath.Join(t.TempDir(), "anchor.conf")
	if err := os.WriteFile(anchorFile, []byte(dnsConformanceTrustedKeys(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), dnsConformanceToolTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, delvPath, "@127.0.0.1", "-p", strconv.Itoa(self.port), "-a", anchorFile,
		"+root="+self.stack.root, question.name, dns.TypeToString[question.rrType])
	output, _ := command.CombinedOutput()
	return string(output)
}

// BIND's delv, anchored to the KSK alone, validates each answer, positive, wildcard and compact denial; anchored to
// another KSK, it validates none.
func TestDnsConformanceDelvValidatesAgainstTheKsk(t *testing.T) {
	delvPath, err := exec.LookPath("delv")
	if err != nil {
		t.Skip("not run, which is no pass: BIND's delv is not on this host's PATH")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		conformance := newDnsConformance(t, ctx)
		version, _ := exec.Command(delvPath, "-v").CombinedOutput()
		t.Logf("validator: %s", strings.TrimSpace(string(version)))
		// a delv built without crypto loads no trusted key and validates nothing: macOS's 9.10.6 is one
		if output := conformance.delv(t, delvPath, conformance.anchor, conformance.questions[0]); strings.Contains(output, "no crypto support") {
			t.Skipf("not run, which is no pass: %s at %s is built without crypto support, so it validates nothing", strings.TrimSpace(string(version)), delvPath)
		}
		for _, question := range conformance.questions {
			output := conformance.delv(t, delvPath, conformance.anchor, question)
			want := "; fully validated"
			if question.negative {
				want = "; negative response, fully validated"
			}
			if !strings.Contains(output, want) {
				t.Errorf("%s (%s %s): delv says\n%s", question.what, question.name, dns.TypeToString[question.rrType], output)
			}
		}
		other := dnsConformanceOtherAnchor(t, conformance.stack.root)
		for _, question := range conformance.questions {
			if output := conformance.delv(t, delvPath, other, question); strings.Contains(output, "fully validated") {
				t.Errorf("%s validated under a KSK that signs nothing:\n%s", question.what, output)
			}
		}
	})
}

// One resolver in a container: its image, the config it reads and where.
type dnsConformanceResolver struct {
	tool       string
	configPath string
	config     string
	// how the container starts the resolver, after the image's entry point
	command []string
}

// The configs the resolvers read: a stub or forward zone for the test root at the edge, through the host's
// loopback, with the KSK as the one trust anchor. Rendered from these with the root, port and anchor.
var dnsConformanceResolvers = []dnsConformanceResolver{
	{
		tool:       "unbound",
		configPath: "/etc/unbound/unbound.conf",
		config: `server:
  interface: 0.0.0.0
  access-control: 0.0.0.0/0 allow
  do-not-query-localhost: no
  trust-anchor: "{{.Anchor}}"
  val-log-level: 2
stub-zone:
  name: "{{.Root}}"
  stub-addr: {{.EdgeAddress}}@{{.Port}}
`,
		command: []string{"unbound", "-d", "-c", "/etc/unbound/unbound.conf"},
	},
	{
		tool:       "knot-resolver",
		configPath: "/etc/knot-resolver/kresd.conf",
		config: `net.listen('0.0.0.0', 53, { kind = 'dns' })
trust_anchors.remove('.')
trust_anchors.add('{{.Anchor}}')
policy.add(policy.suffix(policy.STUB('{{.EdgeAddress}}@{{.Port}}'), {todname('{{.Root}}')}))
`,
		command: []string{"kresd", "-n", "-c", "/etc/knot-resolver/kresd.conf"},
	},
	{
		tool:       "bind",
		configPath: "/etc/bind/named.conf",
		config: `options {
  directory "/var/bind";
  listen-on { any; };
  allow-query { any; };
  recursion yes;
  dnssec-validation yes;
};
trust-anchors {
  "{{.Root}}" static-key {{.AnchorFlags}} 3 13 "{{.AnchorKey}}";
};
zone "{{.Root}}" {
  type forward;
  forward only;
  forwarders { {{.EdgeAddress}} port {{.Port}}; };
};
`,
		command: []string{"named", "-g", "-c", "/etc/bind/named.conf"},
	},
}

// The address a container of image reaches this host by, as its /etc/hosts maps host.docker.internal: an IP, since a
// resolver's stub address must be one.
func dnsConformanceHostAddress(t testing.TB, image string) string {
	t.Helper()
	output, err := exec.Command("docker", "run", "--rm", "--add-host", "host.docker.internal:host-gateway", "--entrypoint", "cat", image, "/etc/hosts").CombinedOutput()
	if err != nil {
		t.Fatalf("%s's /etc/hosts: %v: %s", image, err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "host.docker.internal" && net.ParseIP(fields[0]) != nil {
			return fields[0]
		}
	}
	t.Fatalf("%s maps no host.docker.internal: %s", image, output)
	return ""
}

// The image a tool's container runs, as build.sh tags it.
func dnsConformanceImage(tool string) string {
	return "sidecar-test-dns-" + tool + ":local"
}

// Whether image is on this host, so its container can start with nothing fetched.
func dnsConformanceImagePresent(image string) bool {
	return exec.Command("docker", "image", "inspect", image).Run() == nil
}

// Runs resolver in its container, bound to a loopback port above 50000, and asks it each question with RD and DO,
// requiring its validated answer (AD), and the same with another KSK as its anchor, requiring a SERVFAIL.
func (self *dnsConformance) runResolver(t testing.TB, resolver dnsConformanceResolver) {
	t.Helper()
	for _, anchor := range []*dns.DNSKEY{self.anchor, dnsConformanceOtherAnchor(t, self.stack.root)} {
		configured := &bytes.Buffer{}
		if err := template.Must(template.New(resolver.tool).Parse(resolver.config)).Execute(configured, map[string]any{
			"Root": self.stack.root, "EdgeAddress": dnsConformanceHostAddress(t, dnsConformanceImage(resolver.tool)), "Port": self.port,
			"Anchor": anchor.String(), "AnchorFlags": anchor.Flags, "AnchorKey": anchor.PublicKey,
		}); err != nil {
			t.Fatal(err)
		}
		configFile := filepath.Join(t.TempDir(), filepath.Base(resolver.configPath))
		if err := os.WriteFile(configFile, configured.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		listener, packetConn, port := dnsConformanceListen(t)
		listener.Close()
		packetConn.Close()
		name := fmt.Sprintf("sidecar-test-dns-%s-%d", resolver.tool, port)
		arguments := []string{"run", "-d", "--rm", "--name", name,
			"-p", fmt.Sprintf("127.0.0.1:%d:53/udp", port), "-p", fmt.Sprintf("127.0.0.1:%d:53/tcp", port),
			"--add-host", "host.docker.internal:host-gateway",
			"-v", configFile + ":" + resolver.configPath + ":ro", dnsConformanceImage(resolver.tool)}
		if output, err := exec.Command("docker", append(arguments, resolver.command...)...).CombinedOutput(); err != nil {
			t.Fatalf("%s does not start: %v: %s", name, err, output)
		}
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
		client := &dns.Client{Net: "tcp", Timeout: dnsConformanceToolTimeout}
		for _, question := range self.questions {
			message := &dns.Msg{}
			message.SetQuestion(question.name, question.rrType)
			message.RecursionDesired = true
			message.SetEdns0(1232, true)
			// the resolver may take a moment to load; each try waits on its own answer
			var reply *dns.Msg
			var err error
			for range 10 {
				if reply, _, err = client.Exchange(message, net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
					break
				}
			}
			switch {
			case err != nil:
				t.Errorf("%s: %s: %v", resolver.tool, question.what, err)
			case anchor == self.anchor && (!reply.AuthenticatedData || reply.Rcode != dns.RcodeSuccess):
				t.Errorf("%s: %s is not validated: %v", resolver.tool, question.what, reply)
			case anchor != self.anchor && reply.Rcode != dns.RcodeServerFailure:
				t.Errorf("%s: %s passed under a KSK that signs nothing: %v", resolver.tool, question.what, reply)
			}
		}
		exec.Command("docker", "rm", "-f", name).Run()
	}
}

// Each container resolver validates the edge's answers against the KSK (Unbound's is the monitor's, §12.6), where its
// image is on this host.
func testDnsConformanceResolver(t *testing.T, tool string) {
	image := dnsConformanceImage(tool)
	if _, err := exec.LookPath("docker"); err != nil || !dnsConformanceImagePresent(image) {
		t.Skipf("not run, which is no pass: %s is not on this host; local/dns-conformance/build.sh builds it, fetching its packages", image)
	}
	for _, resolver := range dnsConformanceResolvers {
		if resolver.tool != tool {
			continue
		}
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			newDnsConformance(t, context.Background()).runResolver(t, resolver)
		})
	}
}

func TestDnsConformanceUnboundValidatesAgainstTheKsk(t *testing.T) {
	testDnsConformanceResolver(t, "unbound")
}

func TestDnsConformanceKnotResolverValidatesAgainstTheKsk(t *testing.T) {
	testDnsConformanceResolver(t, "knot-resolver")
}

func TestDnsConformanceBindResolverValidatesAgainstTheKsk(t *testing.T) {
	testDnsConformanceResolver(t, "bind")
}

// zonemaster over the test root. It asks each name server on port 53 alone, which a test edge on this host cannot
// bind, so it runs at T6 over test roots served on 53 (External), and here it is not run.
func TestDnsConformanceZonemasterChecksTheTestRoot(t *testing.T) {
	t.Skip("not run, which is no pass: zonemaster asks port 53 alone, which a test edge here does not bind; it runs at T6 over test roots")
}
