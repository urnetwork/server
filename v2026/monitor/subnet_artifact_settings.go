// Independently enrolled immutable payout objects have a finite review window.
// These inputs identify a read scope; they do not attest chain inclusion or a
// complete historical census. Missing or invalid enrollment remains unknown.
package monitor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"
)

const subnetArtifactEnrollmentSchema = "urnetwork-monitor-subnet-artifacts-v1"
const maximumSubnetArtifactObjects = 4
const maximumSubnetArtifactBytes = 1024 * 1024
const maximumSubnetArtifactSettingsBytes = 16 * 1024

// Review timestamps govern the local expectation, never the age of historical
// evidence. Exactly two HTTPS origins are read separately; this does not prove
// that they use different storage. Objects are supplied by the release owner.
type SubnetArtifactSettings struct {
	Schema       string                      `yaml:"schema"`
	ReviewFrom   string                      `yaml:"review_from"`
	ReviewUntil  string                      `yaml:"review_until"`
	Origins      []string                    `yaml:"origins"`
	Objects      []SubnetArtifactExpectation `yaml:"objects"`
	invalid      bool
	desiredInput [32]byte
}

// Every expected domain and signer comes from local review, never the object
// being tested. A content hash is the existing unsigned canonical artifact
// hash, not the SHA256 of the complete signed HTTP body.
type SubnetArtifactExpectation struct {
	ContentHash     string `yaml:"content_hash"`
	WireSha256      string `yaml:"wire_sha256"`
	DeploymentId    string `yaml:"deployment_id"`
	ChainId         uint64 `yaml:"chain_id"`
	GenesisHash     string `yaml:"genesis_hash"`
	Netuid          uint16 `yaml:"netuid"`
	Coordinator     string `yaml:"coordinator"`
	SettlementVault string `yaml:"settlement_vault"`
	Epoch           uint64 `yaml:"epoch"`
	NoId            uint64 `yaml:"no_id"`
	PolicyHash      string `yaml:"policy_hash"`
	Signer          string `yaml:"signer"`
	StartNumber     uint64 `yaml:"start_number"`
	StartHash       string `yaml:"start_hash"`
	EndNumber       uint64 `yaml:"end_number"`
	EndHash         string `yaml:"end_hash"`
	PayoutRoot      string `yaml:"payout_root"`
}

var subnetArtifactDeployment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Nested values are copied before a caller can reuse its settings snapshot.
func cloneSubnetArtifactSettings(value *SubnetArtifactSettings) *SubnetArtifactSettings {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Origins = append([]string(nil), value.Origins...)
	copy.Objects = append([]SubnetArtifactExpectation(nil), value.Objects...)
	return &copy
}

// Reject aliases, custom tags and excessive nesting before the narrow decoder.
// The containing monitor YAML is already parsed; this walk never expands an alias.
func boundedSubnetArtifactYaml(node *yaml.Node, depth int, remaining, remainingBytes *int) bool {
	*remaining -= 1
	*remainingBytes -= len(node.Value) + len(node.Tag)
	if *remaining < 0 || *remainingBytes < 0 || depth > 4 || node.Alias != nil || node.Anchor != "" {
		return false
	}
	if node.Kind != yaml.MappingNode && node.Kind != yaml.SequenceNode && node.Kind != yaml.ScalarNode {
		return false
	}
	if node.Tag != "!!map" && node.Tag != "!!seq" && node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!timestamp" {
		return false
	}
	for _, child := range node.Content {
		if !boundedSubnetArtifactYaml(child, depth+1, remaining, remainingBytes) {
			return false
		}
	}
	return true
}

// A malformed present block stays visible instead of becoming an empty scope.
// Raw YAML and decoder errors are never retained in a finding or diagnostic.
func subnetArtifactSettingsFromNode(node yaml.Node) *SubnetArtifactSettings {
	if node.Kind == 0 {
		return nil
	}
	invalid := &SubnetArtifactSettings{invalid: true}
	remaining := 512
	remainingBytes := maximumSubnetArtifactSettingsBytes
	if node.Kind != yaml.MappingNode || !boundedSubnetArtifactYaml(&node, 0, &remaining, &remainingBytes) {
		return invalid
	}
	raw, err := yaml.Marshal(&node)
	if err != nil || len(raw) > maximumSubnetArtifactSettingsBytes {
		return invalid
	}
	invalid.desiredInput = sha256.Sum256(raw)
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var value SubnetArtifactSettings
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return invalid
	}
	value.desiredInput = invalid.desiredInput
	return &value
}

// Canonical digests and nonzero addresses keep every equality unambiguous.
func subnetArtifactDigest(value, prefix string) bool {
	if len(value) != len(prefix)+64 || !strings.HasPrefix(value, prefix) || strings.ToLower(value) != value {
		return false
	}
	raw, err := hex.DecodeString(value[len(prefix):])
	return err == nil && len(raw) == 32
}

// Address spelling is the lowercase 20-byte form; no source selects authority.
func subnetArtifactAddress(value string) bool {
	return len(value) == 42 && strings.HasPrefix(value, "0x") && strings.ToLower(value) == value && common.IsHexAddress(value) && common.HexToAddress(value) != (common.Address{})
}

// Only a bare canonical HTTPS origin is admitted. Redirects, userinfo, paths,
// query strings, fragments and environment proxy routing are not enrollment.
func subnetArtifactOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || len(value) > 253 || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.String() != value || strings.ToLower(parsed.Host) != parsed.Host {
		return false
	}
	if parsed.Port() != "" || strings.Contains(parsed.Host, ":") || strings.HasSuffix(parsed.Host, ".") {
		return false
	}
	return regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`).MatchString(parsed.Hostname()) && !strings.Contains(parsed.Hostname(), "..")
}

// Return a fixed reason, never a secret-bearing parse error or desired value.
func (self *SubnetArtifactSettings) validate(now time.Time) string {
	if self == nil {
		return "expectations-missing"
	}
	if self.invalid || self.Schema != subnetArtifactEnrollmentSchema || len(self.Origins) != 2 || len(self.Objects) == 0 || len(self.Objects) > maximumSubnetArtifactObjects {
		return "expectations-invalid"
	}
	from, fromErr := time.Parse(time.RFC3339Nano, self.ReviewFrom)
	until, untilErr := time.Parse(time.RFC3339Nano, self.ReviewUntil)
	if fromErr != nil || untilErr != nil || from.IsZero() || until.IsZero() || from.UTC().Format(time.RFC3339Nano) != self.ReviewFrom || until.UTC().Format(time.RFC3339Nano) != self.ReviewUntil || !from.Before(until) {
		return "expectations-invalid"
	}
	if now.IsZero() || now.Before(from) || !now.Before(until) {
		return "expectations-outside-review-window"
	}
	if !subnetArtifactOrigin(self.Origins[0]) || !subnetArtifactOrigin(self.Origins[1]) || self.Origins[0] == self.Origins[1] {
		return "expectations-invalid"
	}
	seen := map[string]bool{}
	for _, item := range self.Objects {
		if !subnetArtifactDigest(item.ContentHash, "sha256:") || !subnetArtifactDigest(item.WireSha256, "") || seen[item.ContentHash] || !subnetArtifactDeployment.MatchString(item.DeploymentId) || item.ChainId == 0 || item.Netuid == 0 || item.NoId == 0 || !subnetArtifactAddress(item.Coordinator) || !subnetArtifactAddress(item.SettlementVault) || !subnetArtifactAddress(item.Signer) || item.EndNumber < item.StartNumber {
			return "expectations-invalid"
		}
		for _, digest := range []string{item.GenesisHash, item.PolicyHash, item.StartHash, item.EndHash, item.PayoutRoot} {
			if !subnetArtifactDigest(digest, "0x") {
				return "expectations-invalid"
			}
		}
		seen[item.ContentHash] = true
	}
	return ""
}
