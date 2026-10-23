//go:build !linux

// Other platforms leave the optional native identity explicitly unavailable.
package monitor

import (
	"context"
	"errors"
)

// Refuse native binding without fabricating a portable process/unit identity.
func settingsWitnessSelfIdentity(context.Context, SettingsGenerationWitnessSettings) (settingsWitnessIdentity, error) {
	return settingsWitnessIdentity{}, errors.New("settings witness self identity requires Linux")
}
