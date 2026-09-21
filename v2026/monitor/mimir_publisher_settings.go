package monitor

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const mimirPublisherInventoryLimit = 1024 * 1024

// Read only the preference fields from the owning inventory. Decoder errors
// can quote private YAML, so only a fixed load-state enum leaves this helper.
func loadMimirPublisherSettings(warpHome, environment string) MimirPublisherSettings {
	if !mimirPublisherEnvironmentPattern.MatchString(environment) {
		return MimirPublisherSettings{LoadState: "invalid"}
	}
	file, err := os.Open(filepath.Join(warpHome, "xops", environment, "ansible", "inventory.yml"))
	if err != nil {
		return MimirPublisherSettings{LoadState: "unavailable"}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, mimirPublisherInventoryLimit+1))
	if err != nil {
		return MimirPublisherSettings{LoadState: "unavailable"}
	}
	return parseMimirPublisherSettings(data)
}

func parseMimirPublisherSettings(data []byte) MimirPublisherSettings {
	if len(data) > mimirPublisherInventoryLimit {
		return MimirPublisherSettings{LoadState: "invalid"}
	}
	var inventory map[string]struct {
		Hosts map[string]struct {
			PreferredFront string `yaml:"fluent_bit_grafana_preferred_host"`
		} `yaml:"hosts"`
	}
	if err := yaml.Unmarshal(data, &inventory); err != nil || len(inventory) == 0 {
		return MimirPublisherSettings{LoadState: "invalid"}
	}
	fronts := map[string]string{}
	for _, group := range inventory {
		for publisher, configured := range group.Hosts {
			front := strings.TrimSpace(configured.PreferredFront)
			if front == "" {
				continue
			}
			if prior, found := fronts[publisher]; found && prior != front {
				return MimirPublisherSettings{LoadState: "invalid"}
			}
			fronts[publisher] = front
			if len(fronts) > 1024 {
				return MimirPublisherSettings{LoadState: "invalid"}
			}
		}
	}
	return MimirPublisherSettings{LoadState: "ready", PreferredFronts: fronts}
}
