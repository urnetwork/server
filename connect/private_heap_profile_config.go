package connect

import (
	"bytes"
	"errors"
	"io"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/internal/privateheapprofile"
	"gopkg.in/yaml.v3"
)

const privateHeapProfileConfigResource = "connect_private_heap_profile.yml"

// Resolve once at startup. Explicit CLI targets, including "disabled", take
// precedence. Config never changes an existing exchange's accounting scope.
func privateHeapProfileStartupTarget(explicit string) (string, error) {
	target := explicit
	if target == "" {
		resource, err := server.Config.SimpleResource(privateHeapProfileConfigResource)
		if errors.Is(err, server.ErrResourceNotFound) {
			return "", nil
		}
		if err != nil {
			return "", errors.New("private_heap_config_unavailable")
		}
		raw, err := resource.BytesE()
		if err != nil || len(raw) > 4096 {
			return "", errors.New("private_heap_config_unavailable")
		}
		var config struct {
			Target string `yaml:"target"`
		}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil {
			return "", errors.New("private_heap_config_invalid")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return "", errors.New("private_heap_config_invalid")
		}
		target = config.Target
	}
	if target == "disabled" {
		return "", nil
	}
	if !privateheapprofile.ValidTarget(target) {
		return "", errors.New("private_heap_target_invalid")
	}
	return target, nil
}
