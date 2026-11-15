// A source implementation is not approval to deploy a new existing-control
// authority. Absent complete separately pinned metadata the role stays disabled.
package controller

import (
	"errors"
	"path/filepath"
)

// Ordinary host qualification and connection pins are not rewritten by this
// capability. The original role digest remains the existing execution identity.
func validateCompetitionExistingControlConnection(connection competitionExecutorConnection, host CompetitionExecutorHostPolicy, settings competitionDistributedCoordinatorSettings) error {
	value := connection.ExistingControl
	if value == nil {
		return nil
	}
	if settings.ExistingWitnessMaximumAge <= 0 || settings.ExistingWitnessMaximumAge > settings.RequestTimeout ||
		!sha256Pattern.MatchString(connection.AgentConfigSha256) || !sha256Pattern.MatchString(value.ApprovalSha256) || !sha256Pattern.MatchString(value.ResourceBoundarySha256) ||
		!sha256Pattern.MatchString(value.AgentCommandSha256) || !sha256Pattern.MatchString(value.AgentConfigSha256) ||
		!competitionAgentCommandPathPattern.MatchString(value.AgentCommand) || filepath.Clean(value.AgentCommand) != value.AgentCommand ||
		!competitionAgentCommandPathPattern.MatchString(value.AgentConfigPath) || filepath.Clean(value.AgentConfigPath) != value.AgentConfigPath {
		return errors.New("existing control requires independently approved exact original-role and observer pins")
	}
	if host.Transport == "ssh" {
		if !competitionAgentEndpointPattern.MatchString(value.SshEndpoint) || value.SshEndpoint == connection.SshEndpoint {
			return errors.New("existing control requires its distinct closed SSH forced role")
		}
		for _, path := range []string{value.SshIdentityFile, value.SshKnownHostsFile} {
			if err := competitionQualificationRootPath(path, false); err != nil {
				return err
			}
		}
	} else {
		if value.SshEndpoint != "" || value.SshIdentityFile != "" || value.SshKnownHostsFile != "" {
			return ErrConflict
		}
		for _, path := range []string{value.AgentCommand, value.AgentConfigPath} {
			if err := competitionQualificationRootPath(path, false); err != nil {
				return err
			}
		}
		if err := verifyPinnedExecutable(value.AgentCommand, value.AgentCommandSha256); err != nil {
			return err
		}
		digest, _, err := hashRegularFile(value.AgentConfigPath)
		if err != nil || digest != value.AgentConfigSha256 {
			return errors.New("existing control configuration differs from its approved pin")
		}
	}
	return nil
}
