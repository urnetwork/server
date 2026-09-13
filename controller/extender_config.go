package controller

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mathrand "math/rand/v2"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/gossip"

	"github.com/urnetwork/server"
)

// The operator half of the extender trust anchor (connect/EXTENDER.md B1, B4,
// A5).
//
// Everything an operator needs to sign and publish extender records lives in
// one vault resource, `extender.yml`, whose complete key set is:
//
//	root_private_key_hex: <ed25519 seed>
//	root_public_keys_hex:
//	  - <hex public key>
//	network_host: ur.example
//	network_hosts:
//	  - ur.example
//	  - migration.example
//	api_url: https://api.ur.example
//	gossip_identity_key_hex: <ed25519 seed>
//	dns:
//	  enabled: false
//	  hosted_zone_id: <zone>
//	  hosted_zone_name: ur.example
//	  record_name: extender.ur.example
//	  ttl: 60
//	  sample_count: 8
//	  aws_region: <region>
//	  aws_access_key_id: <key id>
//	  aws_secret_access_key: <secret>
//	gossip_dns:
//	  enabled: false
//	  aws_region: <region>
//	  records:
//	    - hosted_zone_name: ur.example
//	      name: gossip.ur.example
//	      source_name: connect.ur.example
//
// The private key signs; the public list is what clients accept, and is a list
// so a key can be rotated by publishing both before the old one is dropped.
// network_host names the space a record belongs to and network_hosts is every
// host the api and connect answer on, which is what an extender is allowed to
// forward to. api_url is the url a forward probe reaches through an extender.
// gossip_identity_key_hex is the mesh identity of the operator's gossip node
// (C6): the service runs under it and hello publishes the peer id derived from
// it, so it must outlive a redeploy or every member loses the operator it was
// told to dial. The dns block is read by the Route 53 publisher (C5) and the
// gossip_dns block by the gossip record setup task (C6); both are carried here
// so operations configure one resource rather than three.
//
// The resource is optional. An operator that has not configured it runs with
// no extender network at all: hello serves no root keys and activation
// refuses, rather than the process failing to start.

// Extender configuration as it appears in the vault resource. Absent fields
// leave their zero value; nothing here is required to parse.
type ExtenderConfig struct {
	RootPrivateKeyHex string   `yaml:"root_private_key_hex"`
	RootPublicKeysHex []string `yaml:"root_public_keys_hex"`
	// the operator's primary host, which is the NetworkHost of every record
	// signed here
	NetworkHost string `yaml:"network_host"`
	// every host the api and connect answer on, normally the primary host and
	// a migration host
	NetworkHosts []string `yaml:"network_hosts"`
	// the public api url a forward probe reaches through an extender
	ApiUrl string `yaml:"api_url"`
	// the ed25519 seed of the operator's gossip node identity (C6, C7)
	GossipIdentityKeyHex string                  `yaml:"gossip_identity_key_hex"`
	Dns                  ExtenderDnsConfig       `yaml:"dns"`
	GossipDns            ExtenderGossipDnsConfig `yaml:"gossip_dns"`
}

// Geo dns publishing (C5), read by the dns half of the publish tick. An unset
// ttl is 60 seconds and an unset sample_count is 8 addresses per set. The
// record name is configured rather than derived from the host, since which
// name an operator serves is an operations decision.
//
// The zone is named either way: hosted_zone_id is the zone verbatim, and
// hosted_zone_name is the zone's domain, which the publisher resolves to an id
// through the aws api once per process. The name is what operations knows, and
// naming it means a zone recreated under a new id does not need this resource
// edited; an explicit id still wins, which is the escape hatch for two zones of
// the same name.
type ExtenderDnsConfig struct {
	Enabled            bool   `yaml:"enabled"`
	HostedZoneId       string `yaml:"hosted_zone_id"`
	HostedZoneName     string `yaml:"hosted_zone_name"`
	RecordName         string `yaml:"record_name"`
	Ttl                int    `yaml:"ttl"`
	SampleCount        int    `yaml:"sample_count"`
	AwsRegion          string `yaml:"aws_region"`
	AwsAccessKeyId     string `yaml:"aws_access_key_id"`
	AwsSecretAccessKey string `yaml:"aws_secret_access_key"`
}

// The gossip service records (C6), read by the setup task that mirrors each
// source name onto its gossip name.
//
// There is no ttl or sample here on purpose: the gossip name is not a rotating
// sample of anything, it is the same address the operator's connect host
// already answers with, so whatever the source carries is what the gossip name
// carries. Credentials come from the host, as the dns block's do when it
// carries none.
type ExtenderGossipDnsConfig struct {
	Enabled   bool                             `yaml:"enabled"`
	AwsRegion string                           `yaml:"aws_region"`
	Records   []*ExtenderGossipDnsRecordConfig `yaml:"records"`
}

// One name to mirror: the source whose A and AAAA sets are read, and the
// gossip name they are written to, both in the zone named here.
type ExtenderGossipDnsRecordConfig struct {
	HostedZoneName string `yaml:"hosted_zone_name"`
	Name           string `yaml:"name"`
	SourceName     string `yaml:"source_name"`
}

// The parse outcome, cached whole so a missing resource is remembered as
// cheaply as a present one and the failure is reported identically at every
// call.
type extenderConfigResult struct {
	config *ExtenderConfig
	err    error
}

var envExtenderConfigOnce = sync.OnceValue(readExtenderConfig)

// EnvExtenderConfig reads `extender.yml` once per process.
//
// The error is the resource being absent or unparseable, which is a deployment
// without an extender network rather than a fault: every caller answers with
// "not configured" instead of failing.
func EnvExtenderConfig() (*ExtenderConfig, error) {
	result := envExtenderConfigOnce()
	return result.config, result.err
}

// Testing_ResetExtenderConfig drops the cached configuration so the next read
// sees an `extender.yml` a test pushed through the vault resolver. Call it
// from the test goroutine only, before anything else reads the configuration.
func Testing_ResetExtenderConfig() {
	envExtenderConfigOnce = sync.OnceValue(readExtenderConfig)
}

func readExtenderConfig() *extenderConfigResult {
	resource, err := server.Vault.SimpleResource("extender.yml")
	if err != nil {
		return &extenderConfigResult{
			err: fmt.Errorf("the extender network is not configured: %w", err),
		}
	}
	config := &ExtenderConfig{}
	if err := resource.UnmarshalYamlE(config); err != nil {
		return &extenderConfigResult{
			err: fmt.Errorf("the extender configuration is not readable: %w", err),
		}
	}
	return &extenderConfigResult{config: config}
}

// RootPrivateKey derives the signing key of the configured seed.
func (self *ExtenderConfig) RootPrivateKey() (ed25519.PrivateKey, error) {
	if strings.TrimSpace(self.RootPrivateKeyHex) == "" {
		return nil, fmt.Errorf("the extender network has no root key")
	}
	seed, err := connect.ParseExtenderKeySeedHex(self.RootPrivateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("the extender root key is not readable: %w", err)
	}
	return connect.ExtenderPrivateKeyFromSeed(seed)
}

// RootPublicKeys is the accepted key list served by hello (B4, C7). Entries
// that are not ed25519 keys are dropped rather than served, since a client
// that cannot parse one has no way to tell a typo from a key it should trust.
func (self *ExtenderConfig) RootPublicKeys() []string {
	rootPublicKeyHexes := []string{}
	for _, rootPublicKeyHex := range self.RootPublicKeysHex {
		rootPublicKeyHex = strings.TrimSpace(rootPublicKeyHex)
		if rootPublicKeyHex == "" {
			continue
		}
		if _, err := connect.ParseExtenderPublicKeyHex(rootPublicKeyHex); err != nil {
			continue
		}
		rootPublicKeyHexes = append(rootPublicKeyHexes, rootPublicKeyHex)
	}
	return rootPublicKeyHexes
}

// GossipIdentityKeySeed is the ed25519 seed the operator's gossip node runs
// its mesh identity under (C6). It is stored as a seed rather than as a key so
// that one hex form serves every ed25519 identity in the design (B1).
//
// An absent or unreadable key is an error naming the key, since the gossip
// service cannot pick one for itself: a generated identity would change on
// every redeploy and every member would be left dialing a peer id that no
// longer answers.
func (self *ExtenderConfig) GossipIdentityKeySeed() ([]byte, error) {
	if strings.TrimSpace(self.GossipIdentityKeyHex) == "" {
		return nil, fmt.Errorf("the extender network has no gossip_identity_key_hex")
	}
	seed, err := connect.ParseExtenderKeySeedHex(self.GossipIdentityKeyHex)
	if err != nil {
		return nil, fmt.Errorf("the extender gossip_identity_key_hex is not readable: %w", err)
	}
	return seed, nil
}

// GossipPeerId is the mesh identity hello serves (C7), which is what lets a
// member demand the operator it meant to reach at the other end of its dial
// (D3). A member with no peer id makes no operator dial at all, so an operator
// with no gossip key answers with none rather than with something unverifiable.
func (self *ExtenderConfig) GossipPeerId() (string, error) {
	seed, err := self.GossipIdentityKeySeed()
	if err != nil {
		return "", err
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		return "", err
	}
	peerId, err := gossip.PeerIdForExtenderPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	return peerId.String(), nil
}

// AllowedHosts is the operator half of an extender's whitelist (A5): for every
// host the api and connect answer on, the host itself and one wildcard level
// under it. An extender forwards only to these, so the list is what an
// activation hands back for the extender to configure itself with.
//
// The primary host is always included, even when network_hosts is empty or
// does not repeat it, so an operator that configured only network_host still
// gets a usable whitelist.
func (self *ExtenderConfig) AllowedHosts() []string {
	allowedHosts := []string{}
	add := func(host string) {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if host == "" {
			return
		}
		for _, pattern := range []string{host, "*." + host} {
			if !slices.Contains(allowedHosts, pattern) {
				allowedHosts = append(allowedHosts, pattern)
			}
		}
	}
	add(self.NetworkHost)
	for _, networkHost := range self.NetworkHosts {
		add(networkHost)
	}
	return allowedHosts
}

// ApiHost is the host of the public api url, which is the destination every
// probe asks an extender to forward to. It must be on the extender's
// whitelist, which AllowedHosts is what put it there.
func (self *ExtenderConfig) ApiHost() (string, error) {
	if strings.TrimSpace(self.ApiUrl) == "" {
		return "", fmt.Errorf("the extender network has no api url")
	}
	apiUrl, err := url.Parse(self.ApiUrl)
	if err != nil {
		return "", fmt.Errorf("the extender network api url is not readable: %w", err)
	}
	if apiUrl.Hostname() == "" {
		return "", fmt.Errorf("the extender network api url has no host")
	}
	return apiUrl.Hostname(), nil
}

// ProbeServerName picks the outer sni one probe fronts an extender with (A10).
//
// A bundled spoof domain is the realistic choice, since that is what a client
// dial presents. The bundled list ships empty, so until operations provide it
// the name is a random label under the operator's own host: still a name the
// extender will issue a certificate for, still different on every probe, and
// never a name that belongs to someone else.
func (self *ExtenderConfig) ProbeServerName() (string, error) {
	spoofDomains := connect.SpoofDomains()
	if 0 < len(spoofDomains) {
		return spoofDomains[mathrand.IntN(len(spoofDomains))], nil
	}
	networkHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(self.NetworkHost), "."))
	if networkHost == "" {
		return "", fmt.Errorf("the extender network has no host")
	}
	label := make([]byte, 8)
	if _, err := rand.Read(label); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%s", hex.EncodeToString(label), networkHost), nil
}
