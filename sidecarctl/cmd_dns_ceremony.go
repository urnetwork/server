// `sidecarctl dns ceremony prepare|assemble|verify` is the officers' tool for §12.6's Keys ceremony (D-DNS-5,
// R-DNS-10; DNS-M5-19): a quarter's DNSKEY windows, signed ahead by the KSK on an offline HSM with two people
// present. The three are short commands over dnsservice's CeremonyPrepare, CeremonyAssemble and
// VerifyDnsKeyWindowFile, and they work on files alone: no env, no vault, no Postgres and no network, so they run on
// the ceremony's offline machine from a copy of config/<env>/dns.yml.
//
//   - prepare reads dns.yml, the KSK's public DNSKEY and the three ZSKs' (current, next and standby), each a file
//     holding one presentation-form RDATA, lays --from to --until out in windows, and writes the plan: each
//     window's exact signing input and its SHA-256, which it also prints for the officers to read against the
//     HSM's own.
//   - the HSM signs each window's bytes outside this tool. Nothing here takes a KSK's private key.
//   - assemble reads the plan and the signatures, one file a window in the plan's order, each raw r||s or DER,
//     verifies each against the plan's KSK, and writes the window file for vault
//     (dns-key-windows-<root>.json), keeping --previous's windows that have not ended. A wrong signature, a KSK
//     dns.yml does not anchor or a gap ahead writes nothing.
//   - verify rereads a window file against dns.yml's anchors and prints where it serves.
//
// A file a command writes is created, never replaced: one that exists is refused, so no run overwrites a plan the
// HSM has signed or a window file bound for vault.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/urnetwork/sidecar/dnsservice"
)

// The three commands' registrations.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "dns ceremony prepare",
		usage:   "--dns-yml=<file> [--root=<root>] --ksk=<file> --zsk-current=<file> --zsk-next=<file> --zsk-standby=<file> --from=<RFC 3339> --until=<RFC 3339> --out=<plan file>",
		summary: "Lay a span out in DNSKEY windows and write the plan: each window's exact signing input for the offline KSK and its SHA-256. Reads files only.",
		run:     runDnsCeremonyPrepare,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "dns ceremony assemble",
		usage:   "--dns-yml=<file> --plan=<plan file> [--previous=<window file>] --out=<window file> <signature file>...",
		summary: "Verify the HSM's signatures, one file a window in the plan's order, against the plan's KSK and write the window file for vault. Reads files only.",
		run:     runDnsCeremonyAssemble,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "dns ceremony verify",
		usage:   "--dns-yml=<file> <window file>",
		summary: "Reread a DNSKEY window file against dns.yml's anchors and print where it serves. Reads files only.",
		run:     runDnsCeremonyVerify,
	})
}

// How the ceremony commands read and write. Tests replace the clock.
type dnsCeremonySettings struct {
	// The most any one input file may hold: dns.yml, a key, a plan, a signature or a window file. A quarter's plan
	// is tens of kilobytes; a megabyte leaves room and keeps a wrong path, a device say, from being read whole.
	inputMaxByteCount int64
	// The mode a written plan or window file is created with: its owner alone, as vault's files are.
	outputFileMode os.FileMode
	// The time assemble judges a gap ahead at.
	now func() time.Time
}

// The settings of a run. A caller changes its own copy.
func defaultDnsCeremonySettings() *dnsCeremonySettings {
	return &dnsCeremonySettings{
		inputMaxByteCount: 1 << 20,
		outputFileMode:    0o600,
		now:               time.Now,
	}
}

// Reads one input file whole, refusing one over the settings' bound. what names the file's part in an error.
func (self *dnsCeremonySettings) readInput(what string, path string) ([]byte, error) {
	if path == "" {
		return nil, sidecarctlUsageErrorf("%s names no file", what)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, self.inputMaxByteCount+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if self.inputMaxByteCount < int64(len(data)) {
		return nil, fmt.Errorf("%s: %s holds more than %d bytes", what, path, self.inputMaxByteCount)
	}
	return data, nil
}

// Creates path with data, refusing a path that exists, and removes what it made if the write fails, so a path
// either holds the whole file or nothing.
func (self *dnsCeremonySettings) writeOutput(what string, path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, self.outputFileMode)
	if err != nil {
		return fmt.Errorf("%s was not written: %w", what, err)
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("%s was not written: %w", what, err)
	}
	return nil
}

// dns.yml from --dns-yml.
func (self *dnsCeremonySettings) readDnsSettings(path string) (*dnsservice.DnsSettings, error) {
	data, err := self.readInput("--dns-yml", path)
	if err != nil {
		return nil, err
	}
	settings, err := dnsservice.LoadDnsSettings(data)
	if err != nil {
		return nil, fmt.Errorf("--dns-yml: %w", err)
	}
	return settings, nil
}

// One DNSKEY's RDATA from a file that holds it on one line, with nothing else but blank lines.
func (self *dnsCeremonySettings) readDnskey(what string, path string) (string, error) {
	data, err := self.readInput(what, path)
	if err != nil {
		return "", err
	}
	lines := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		return "", fmt.Errorf("%s: %s holds %d lines, want one DNSKEY RDATA", what, path, len(lines))
	}
	return lines[0], nil
}

// A flag's time, in RFC 3339.
func parseDnsCeremonyTime(what string, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, sidecarctlUsageErrorf("%s=%q is not an RFC 3339 time, such as 2027-01-01T00:00:00Z", what, value)
	}
	return parsed, nil
}

// What a window file serves, as the officers read it: each release and each gap.
func printDnsCeremonyCoverage(out io.Writer, coverage *dnsservice.DnsKeyWindowCoverage) {
	fmt.Fprintf(out, "served from %s\n", coverage.From.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "the next ceremony's windows must be released by %s\n", coverage.Until.UTC().Format(time.RFC3339))
	if len(coverage.Gaps) == 0 {
		fmt.Fprintf(out, "gaps: none\n")
	}
	for _, gap := range coverage.Gaps {
		fmt.Fprintf(out, "gap: no window with enough left from %s until %s\n", gap.From.UTC().Format(time.RFC3339), gap.Until.UTC().Format(time.RFC3339))
	}
}

// `sidecarctl dns ceremony prepare`.
func runDnsCeremonyPrepare(args []string, out io.Writer) error {
	return runDnsCeremonyPrepareWithSettings(args, defaultDnsCeremonySettings(), out)
}

// Parses the command line, lays the span out and writes the plan. Nothing is written unless the whole plan is.
func runDnsCeremonyPrepareWithSettings(args []string, settings *dnsCeremonySettings, out io.Writer) error {
	flags := flag.NewFlagSet("dns ceremony prepare", flag.ContinueOnError)
	dnsYmlFlag := flags.String("dns-yml", "", "")
	rootFlag := flags.String("root", "", "")
	kskFlag := flags.String("ksk", "", "")
	zskCurrentFlag := flags.String("zsk-current", "", "")
	zskNextFlag := flags.String("zsk-next", "", "")
	zskStandbyFlag := flags.String("zsk-standby", "", "")
	fromFlag := flags.String("from", "", "")
	untilFlag := flags.String("until", "", "")
	outFlag := flags.String("out", "", "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return sidecarctlUsageErrorf("takes flags only, got %q", positionals)
	}
	if *outFlag == "" {
		return sidecarctlUsageErrorf("--out names the plan file to write")
	}
	from, err := parseDnsCeremonyTime("--from", *fromFlag)
	if err != nil {
		return err
	}
	until, err := parseDnsCeremonyTime("--until", *untilFlag)
	if err != nil {
		return err
	}
	dnsSettings, err := settings.readDnsSettings(*dnsYmlFlag)
	if err != nil {
		return err
	}
	root, err := dnsYmlRoot(dnsSettings.Roots, *rootFlag)
	if err != nil {
		return err
	}
	request := &dnsservice.DnsCeremonyRequest{Root: root, From: from, Until: until}
	if request.Ksk, err = settings.readDnskey("--ksk", *kskFlag); err != nil {
		return err
	}
	for _, zsk := range []struct {
		what string
		path string
	}{
		{what: "--zsk-current", path: *zskCurrentFlag},
		{what: "--zsk-next", path: *zskNextFlag},
		{what: "--zsk-standby", path: *zskStandbyFlag},
	} {
		rdata, err := settings.readDnskey(zsk.what, zsk.path)
		if err != nil {
			return err
		}
		request.Zsks = append(request.Zsks, rdata)
	}
	plan, err := dnsservice.CeremonyPrepare(&dnsSettings.Keys, request)
	if err != nil {
		return fmt.Errorf("no plan was written: %w", err)
	}
	data, err := plan.Marshal()
	if err != nil {
		return fmt.Errorf("no plan was written: %w", err)
	}
	if err := settings.writeOutput("the plan", *outFlag, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "plan for %s: %d windows, each signing the KSK and the current, next and standby ZSKs\n", plan.Root, len(plan.Windows))
	for index, window := range plan.Windows {
		fmt.Fprintf(out, "window %d: %s to %s, signing input sha256 %s\n",
			index, window.NotBefore.UTC().Format(time.RFC3339), window.NotAfter.UTC().Format(time.RFC3339), window.SigningInputSha256)
	}
	fmt.Fprintf(out, "wrote %s; the HSM signs each window's signing_input with the KSK, ECDSA P-256 over SHA-256, one signature a window in this order\n", *outFlag)
	return nil
}

// `sidecarctl dns ceremony assemble`.
func runDnsCeremonyAssemble(args []string, out io.Writer) error {
	return runDnsCeremonyAssembleWithSettings(args, defaultDnsCeremonySettings(), out)
}

// Parses the command line, verifies the signatures and writes the window file. Nothing is written unless every
// window verifies and the file serves with no gap ahead.
func runDnsCeremonyAssembleWithSettings(args []string, settings *dnsCeremonySettings, out io.Writer) error {
	flags := flag.NewFlagSet("dns ceremony assemble", flag.ContinueOnError)
	dnsYmlFlag := flags.String("dns-yml", "", "")
	planFlag := flags.String("plan", "", "")
	previousFlag := flags.String("previous", "", "")
	outFlag := flags.String("out", "", "")
	signaturePaths, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if *outFlag == "" {
		return sidecarctlUsageErrorf("--out names the window file to write")
	}
	dnsSettings, err := settings.readDnsSettings(*dnsYmlFlag)
	if err != nil {
		return err
	}
	planData, err := settings.readInput("--plan", *planFlag)
	if err != nil {
		return err
	}
	plan, err := dnsservice.ParseDnsCeremonyPlan(planData)
	if err != nil {
		return fmt.Errorf("--plan: %w", err)
	}
	if len(signaturePaths) != len(plan.Windows) {
		return sidecarctlUsageErrorf("the plan has %d windows and %d signature files were given, want one a window in the plan's order", len(plan.Windows), len(signaturePaths))
	}
	signatures := [][]byte{}
	for index, path := range signaturePaths {
		signature, err := settings.readInput(fmt.Sprintf("window %d's signature", index), path)
		if err != nil {
			return err
		}
		signatures = append(signatures, signature)
	}
	var previous *dnsservice.DnsKeyWindowFile
	if *previousFlag != "" {
		previousData, err := settings.readInput("--previous", *previousFlag)
		if err != nil {
			return err
		}
		if previous, err = dnsservice.ParseDnsKeyWindowFile(previousData); err != nil {
			return fmt.Errorf("--previous: %w", err)
		}
	}
	file, coverage, err := dnsservice.CeremonyAssemble(dnsSettings, plan, signatures, previous, settings.now())
	if err == nil && (file == nil || coverage == nil) {
		err = errors.New("assemble answered no file")
	}
	if err != nil {
		return fmt.Errorf("no window file was written: %w", err)
	}
	data, err := file.Marshal()
	if err != nil {
		return fmt.Errorf("no window file was written: %w", err)
	}
	if err := settings.writeOutput("the window file", *outFlag, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "window file for %s: %d windows, %d of them from this plan, each verified against the plan's KSK\n", file.Root, len(file.Windows), len(plan.Windows))
	printDnsCeremonyCoverage(out, coverage)
	fmt.Fprintf(out, "wrote %s; it goes to vault as %s\n", *outFlag, dnsservice.DnsKeyWindowFileName(file.Root))
	return nil
}

// `sidecarctl dns ceremony verify`.
func runDnsCeremonyVerify(args []string, out io.Writer) error {
	return runDnsCeremonyVerifyWithSettings(args, defaultDnsCeremonySettings(), out)
}

// Parses the command line and rereads the window file. A file that does not verify prints nothing.
func runDnsCeremonyVerifyWithSettings(args []string, settings *dnsCeremonySettings, out io.Writer) error {
	flags := flag.NewFlagSet("dns ceremony verify", flag.ContinueOnError)
	dnsYmlFlag := flags.String("dns-yml", "", "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return sidecarctlUsageErrorf("want one window file, got %q", positionals)
	}
	dnsSettings, err := settings.readDnsSettings(*dnsYmlFlag)
	if err != nil {
		return err
	}
	data, err := settings.readInput("the window file", positionals[0])
	if err != nil {
		return err
	}
	file, err := dnsservice.ParseDnsKeyWindowFile(data)
	if err != nil {
		return fmt.Errorf("%s does not verify: %w", positionals[0], err)
	}
	coverage, err := dnsservice.VerifyDnsKeyWindowFile(dnsSettings, file)
	if err == nil && coverage == nil {
		err = errors.New("verify answered no coverage")
	}
	if err != nil {
		return fmt.Errorf("%s does not verify: %w", positionals[0], err)
	}
	fmt.Fprintf(out, "%s verifies: %d windows for %s, each signed by an anchor dns.yml names for it\n", positionals[0], len(file.Windows), file.Root)
	printDnsCeremonyCoverage(out, coverage)
	return nil
}
