package evshost

// The block's config (QD-854; §11.6's format row, D-PHASE-10, FS-72): config/<env>/evs.yml, which evs's block and
// taskworker's evs jobs each read once, when they start, from the config their block was deployed with.
//
//	# the storage format this block runs as: 2 from M10's release, 1 in a rollback's
//	instance_format: 2
//
// A rollback to format 1 is a configuration of this binary, not an older build: a block deployed with
// instance_format: 1 runs format 1's rules and its guard over format-2 data (evsservice/format1_guard.go), refusing
// format-2 pushes, purging nothing and writing nothing to a format-2 repo, where a build from before format 2 holds no
// such guard. The value becomes evsservice.EvsSettings.InstanceFormat, which no caller of a block sets any other way.
//
// An absent file, or one that names no instance_format, is this code's own format, EvsSettings' default. Everything
// else is an error and the block does not start: a file that does not read or parse, a key this code does not know,
// or a value that is not the whole number 1 or 2. A rollback's file read as the default would run format 2 where its
// operator deployed format 1, so nothing here guesses.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
)

// The config's name under config/<env>.
const BlockConfigFileName = "evs.yml"

// A block config this code does not run under.
var ErrBlockConfig = errors.New("evs block config")

// What a block's config names.
type BlockConfig struct {
	// The storage format the block runs as (evsservice.EvsSettings.InstanceFormat): sidecar.EvsRepoFormat1 or
	// sidecar.EvsRepoFormat2.
	InstanceFormat uint32
}

// The file's keys. instance_format stays a node so a fraction, a string or a list is refused, never converted.
type blockConfigYaml struct {
	InstanceFormat yaml.Node `yaml:"instance_format"`
}

// What an absent file reads: this code's own format.
func DefaultBlockConfig() *BlockConfig {
	return &BlockConfig{
		InstanceFormat: evsservice.DefaultEvsSettings().InstanceFormat,
	}
}

// The config a file's bytes name. See the file header for what is an error.
func ParseBlockConfig(file []byte) (*BlockConfig, error) {
	blockConfig := DefaultBlockConfig()
	y := &blockConfigYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(file))
	decoder.KnownFields(true)
	if err := decoder.Decode(y); err != nil {
		if errors.Is(err, io.EOF) {
			// nothing but comments: every key at its default
			return blockConfig, nil
		}
		return nil, fmt.Errorf("%w: %s does not parse: %w", ErrBlockConfig, BlockConfigFileName, err)
	}
	// one document, so a second cannot carry a format the first hides
	if err := decoder.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %s holds more than one document", ErrBlockConfig, BlockConfigFileName)
	}
	node := &y.InstanceFormat
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	if node.Kind == 0 {
		// no key
		return blockConfig, nil
	}
	instanceFormat := uint32(0)
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!int" || node.Decode(&instanceFormat) != nil {
		return nil, fmt.Errorf("%w: %s sets an instance_format that is not a whole number", ErrBlockConfig, BlockConfigFileName)
	}
	if instanceFormat != sidecar.EvsRepoFormat1 && instanceFormat != sidecar.EvsRepoFormat2 {
		return nil, fmt.Errorf("%w: %s sets instance_format %d, which is neither %d nor %d", ErrBlockConfig, BlockConfigFileName,
			instanceFormat, sidecar.EvsRepoFormat1, sidecar.EvsRepoFormat2)
	}
	blockConfig.InstanceFormat = instanceFormat
	return blockConfig, nil
}

// The config this process's block was deployed with. See the file header for what is an error.
func LoadBlockConfig() (blockConfig *BlockConfig, returnErr error) {
	failed := func(err any) {
		blockConfig, returnErr = nil, fmt.Errorf("%w: %s does not read: %v", ErrBlockConfig, BlockConfigFileName, err)
	}
	if r := server.HandleError(func() {
		resource, err := server.Config.SimpleResource(BlockConfigFileName)
		if err != nil {
			if !errors.Is(err, server.ErrResourceNotFound) || errors.Is(err, server.ErrResourceUnavailable) {
				failed(err)
				return
			}
			blockConfig = DefaultBlockConfig()
			return
		}
		file, err := resource.BytesE()
		if err != nil {
			failed(err)
			return
		}
		blockConfig, returnErr = ParseBlockConfig(file)
	}); r != nil {
		failed(r)
	}
	return
}

// The engine's settings as a block under this config runs them: settings, the defaults when nil, copied so the
// caller's stay its own, with the config's instance format whatever settings held.
func (self *BlockConfig) EvsSettings(settings *evsservice.EvsSettings) *evsservice.EvsSettings {
	if settings == nil {
		settings = evsservice.DefaultEvsSettings()
	}
	evsSettings := *settings
	evsSettings.InstanceFormat = self.InstanceFormat
	return &evsSettings
}

// One Testing_WithBlockConfig at a time a process.
var testingBlockConfigLock sync.Mutex

// Runs run with file standing in for the block's config, as a block deployed with it reads it: what a test starts a
// block or loads a job's settings under. One at a time a process, so two starts never read each other's file; run
// starts no other.
func Testing_WithBlockConfig(file string, run func()) {
	testingBlockConfigLock.Lock()
	defer testingBlockConfigLock.Unlock()
	defer server.Config.PushSimpleResource(BlockConfigFileName, []byte(file))()
	run()
}
