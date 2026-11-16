//go:build linux

// Operator provisioning uses the same closed documents that native admission
// verifies. Rendering never inspects a host, starts a service or admits a job.
package competitionagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// Bytes are base64 in JSON so a transport cannot normalize TOML/nft whitespace.
type RuntimeProvisioningDocument struct {
	Content []byte `json:"content"`
	Sha256  string `json:"sha256"`
}

// This is a configuration plan, not a native qualification/admission receipt.
type RuntimeProvisioningDocuments struct {
	Schema           int                         `json:"schema"`
	Kind             string                      `json:"kind"`
	RuntimeQualified bool                        `json:"runtime_qualified"`
	Boundary         RuntimeResourceBoundary     `json:"boundary"`
	Engine           RuntimeProvisioningDocument `json:"engine"`
	Containerd       RuntimeProvisioningDocument `json:"containerd"`
	Network          RuntimeProvisioningDocument `json:"network"`
}

// Empty hashes are permitted only for the three documents being generated.
// Their actual hashes complete the returned metadata, without modifying input.
// Native file/kernel/namespace checks still belong to VerifyRuntimeResourceBoundary.
func RenderRuntimeProvisioningDocuments(boundary RuntimeResourceBoundary, scope RuntimeResourceScope, settings RuntimeResourceBoundarySettings) (*RuntimeProvisioningDocuments, error) {
	if err := validateRuntimeResourceBoundarySettings(settings); err != nil {
		return nil, err
	}
	engine, err := json.MarshalIndent(runtimeBoundaryEngineDocument(boundary, settings), "", "  ")
	if err != nil {
		return nil, err
	}
	engine = append(engine, '\n')
	containerd, network := runtimeBoundaryContainerdDocument(boundary), runtimeBoundaryNetworkPolicy()
	result := &RuntimeProvisioningDocuments{Schema: Schema, Kind: "runtime-resource-provisioning-documents/v1", RuntimeQualified: false}
	for _, output := range []struct {
		pin      *Pin
		content  []byte
		document *RuntimeProvisioningDocument
	}{{pin: &boundary.EngineConfig, content: engine, document: &result.Engine},
		{pin: &boundary.ContainerdConfig, content: containerd, document: &result.Containerd},
		{pin: &boundary.NetworkPolicy, content: network, document: &result.Network}} {
		digest := sha256.Sum256(output.content)
		sha := hex.EncodeToString(digest[:])
		if output.pin.Sha256 != "" && output.pin.Sha256 != sha {
			return nil, errors.New("provisioning output conflicts with an already declared document digest")
		}
		output.pin.Sha256 = sha
		*output.document = RuntimeProvisioningDocument{Content: output.content, Sha256: sha}
	}
	if err := boundary.validate(scope); err != nil {
		return nil, err
	}
	if err := validateRuntimeBoundaryDocuments(boundary, engine, containerd, network, settings); err != nil {
		return nil, err
	}
	result.Boundary = boundary
	return result, nil
}
