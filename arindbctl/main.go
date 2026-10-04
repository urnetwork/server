// Refreshes the GeoLite2 source before building the ARIN exception database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// The all-release runner stamps this from its selected Server source version.
var Version = "development"

// Paths are explicit so a release uses its selected source and secret files.
type commandOptions struct {
	geoipConfig  string
	credentials  string
	rules        string
	geolite2     string
	source       string
	output       string
	timeout      time.Duration
	relayFeeds   bool
	labelFeeds   bool
	hostingFeeds bool
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runCommand(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "arindbctl:", err)
		os.Exit(1)
	}
}

// Commands expose independent refreshes and one atomic release bundle operation.
func runCommand(ctx context.Context, args []string, output io.Writer) error {
	return runCommandWithDependencies(ctx, args, output, commandDependencies{geoipUpdate: runGeoipUpdate})
}

// Dependencies belong to one command; synthetic tests never replace a global
// HTTP transport or updater used by another invocation.
type commandDependencies struct {
	geoipUpdate    geoipUpdater
	arinClient     *http.Client
	evidenceClient *http.Client
}

// Exercises the complete publication path with explicit command-owned I/O.
func runCommandWithDependencies(ctx context.Context, args []string, output io.Writer, dependencies commandDependencies) error {
	if len(args) == 0 {
		return errors.New("usage: arindbctl {geolite2 refresh|arin refresh|build|augment-subscribers|audit-subscriber-catalog|refresh-subscriber-evidence|refresh} [flags]")
	}
	command := args[0]
	args = args[1:]
	if command == "geolite2" || command == "arin" {
		if len(args) == 0 || args[0] != "refresh" {
			return errors.New("expected refresh subcommand")
		}
		args = args[1:]
	}
	options := commandOptions{}
	flags := flag.NewFlagSet("arindbctl", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.geoipConfig, "geoip-config", "", "protected MaxMind Vault YAML path (vault/mm-geoip.yml)")
	flags.StringVar(&options.credentials, "credentials", "", "protected ARIN YAML credential path")
	flags.StringVar(&options.rules, "rules", "", "reviewed ARIN classification rule YAML path")
	flags.StringVar(&options.geolite2, "geolite2", "", "GeoLite2-City source for country correlation and reviewed-country discrimination")
	flags.StringVar(&options.source, "source", "", "ARIN bulk XML input for an offline build")
	flags.StringVar(&options.output, "output", "", "new output directory; existing directories are never replaced")
	flags.DurationVar(&options.timeout, "timeout", time.Hour, "total command deadline")
	flags.BoolVar(&options.relayFeeds, "relay-geofeeds", false, "also pin Apple Private Relay and Cloudflare egress geofeeds as reviewed VPN address lists")
	flags.BoolVar(&options.hostingFeeds, "hosting-prefixes", false, "also pin AWS EC2, Google Cloud, AzureCloud, Oracle, DigitalOcean, Linode and Vultr published prefixes as hosting evidence")
	flags.BoolVar(&options.labelFeeds, "label-sources", false, "also pin bgp.tools ASN classes and tags and APNIC Labs user estimates for the audit's independent validation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || options.output == "" || options.timeout <= 0 {
		return errors.New("output and a positive timeout are required; positional arguments are not accepted")
	}
	ctx, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()
	return publishDirectory(options.output, func(stage string) error {
		switch command {
		case "geolite2":
			return refreshGeolite2(ctx, options.geoipConfig, stage, dependencies.geoipUpdate)
		case "arin":
			return refreshArin(ctx, options.credentials, filepath.Join(stage, "arin_db.xml"), dependencies.arinClient)
		case "build":
			return buildArinDatabase(ctx, options.source, options.geolite2, options.rules, stage)
		case "augment-subscribers":
			return augmentSubscriberDatabase(ctx, options.source, options.rules, options.geolite2, stage, time.Now().UTC())
		case "audit-subscriber-catalog":
			return auditSubscriberCatalog(ctx, options.rules, options.geolite2, stage, time.Now().UTC())
		case "refresh-subscriber-evidence":
			return refreshSubscriberEvidence(ctx, options.rules, options.relayFeeds, options.labelFeeds, options.hostingFeeds, stage, dependencies.evidenceClient, time.Now().UTC())
		case "refresh":
			// Neither database becomes visible until both were built and verified.
			geoDir := filepath.Join(stage, "mmdb")
			if err := os.Mkdir(geoDir, 0o755); err != nil {
				return err
			}
			if err := refreshGeolite2(ctx, options.geoipConfig, geoDir, dependencies.geoipUpdate); err != nil {
				return err
			}
			arinDir := filepath.Join(stage, "arindb")
			if err := os.Mkdir(arinDir, 0o755); err != nil {
				return err
			}
			raw, err := os.CreateTemp(stage, ".arin-source-*.xml")
			if err != nil {
				return err
			}
			rawPath := raw.Name()
			if err := raw.Close(); err != nil {
				return err
			}
			defer os.Remove(rawPath)
			if err := refreshArin(ctx, options.credentials, rawPath, dependencies.arinClient); err != nil {
				return err
			}
			return buildArinDatabase(ctx, rawPath, filepath.Join(geoDir, "geolite2.mmdb"), options.rules, arinDir)
		default:
			return errors.New("unknown command")
		}
	})
}

// A same-filesystem directory rename publishes all related artifacts together.
// Staging paths are owned by this invocation and never selected as config versions.
func publishDirectory(output string, build func(string) error) error {
	output, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(output); err == nil {
		return errors.New("output already exists; select a new version directory")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".ip-refresh-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := build(stage); err != nil {
		return err
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(output); err == nil {
		return errors.New("output appeared during refresh")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(stage, output)
}
