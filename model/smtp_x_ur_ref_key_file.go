package model

// The X-UR-Ref keys' vault file (§14.6, §14.14's vault X-UR-Ref keys; SMTP-M9-10, SMTP-M9-15): the keys a send's
// X-UR-Ref is issued and verified under, by id, and the one new references are issued under. smtpedge reads it to
// issue and verify, and mxedge's platform listener to verify the references feedback-loop reports and role mail
// carry, which its report intake's attribution joins on (plan-SMTP-report-intake-home). Each block mounts it among
// its secret_files (warp/services/sidecar-blocks), and nothing reads it from the repository:
//
//	issue_key_id: <1-255>
//	keys:
//	  - {id: <1-255>, key: <base64>}
//
// A refusal names the file and the part, never a key.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// The file among a block's secret_files.
const SmtpXUrRefKeyFileName = "smtpedge-x-ur-ref.yml"

// A key file that is missing, does not parse or holds a key the settings refuse.
var ErrSmtpXUrRefKeyFileRefused = errors.New("x-ur-ref key file refused")

// The file's yaml.
type smtpXUrRefKeyFileYaml struct {
	IssueKeyId int `yaml:"issue_key_id"`
	Keys       []struct {
		Id  int    `yaml:"id"`
		Key string `yaml:"key"`
	} `yaml:"keys"`
}

// Lays the key file in data over settings: its keys and the one new references are issued under, which
// settings.Validate then checks with every other field. A refusal names the part, never a key.
func ApplySmtpXUrRefKeyFile(settings *smtpedgeservice.SmtpEdgeSettings, data []byte) error {
	refuse := func(part string) error {
		return fmt.Errorf("%w: %s %s", ErrSmtpXUrRefKeyFileRefused, SmtpXUrRefKeyFileName, part)
	}
	file := &smtpXUrRefKeyFileYaml{}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(file); err != nil {
		// the decoder's words can quote a key written where the format wants another kind
		return refuse("does not parse")
	}
	keys := []*smtpedgeservice.SmtpXUrRefKey{}
	for _, entry := range file.Keys {
		if entry.Id < 1 || 255 < entry.Id {
			return refuse("holds a key id past one octet")
		}
		key, err := base64.StdEncoding.DecodeString(entry.Key)
		if err != nil {
			return refuse("holds a key that is not base64")
		}
		keys = append(keys, &smtpedgeservice.SmtpXUrRefKey{KeyId: uint8(entry.Id), Key: key})
	}
	settings.XUrRefKeys = keys
	settings.XUrRefIssueKeyId = file.IssueKeyId
	if err := settings.Validate(); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrSmtpXUrRefKeyFileRefused, SmtpXUrRefKeyFileName, err)
	}
	return nil
}

// Lays the process's vault's key file over settings (ApplySmtpXUrRefKeyFile). A file the vault does not hold is
// refused as missing.
func LoadSmtpXUrRefKeyFile(settings *smtpedgeservice.SmtpEdgeSettings) error {
	resource, err := server.Vault.SimpleResource(SmtpXUrRefKeyFileName)
	if err != nil {
		return fmt.Errorf("%w: no %s", ErrSmtpXUrRefKeyFileRefused, SmtpXUrRefKeyFileName)
	}
	data, err := resource.BytesE()
	if err != nil {
		return fmt.Errorf("%w: no %s", ErrSmtpXUrRefKeyFileRefused, SmtpXUrRefKeyFileName)
	}
	return ApplySmtpXUrRefKeyFile(settings, data)
}
