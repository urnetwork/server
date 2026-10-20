// Private receipt provenance reuses existing admission ownership without reads
// or admission changes. It is correlation metadata, not file-writer proof.
package monitor

// A complete retained tuple can be compared with independent native authority.
// The receipt hash binds these declared values, not their physical writer.
type pgSampleWriterGeneration struct {
	Schema int               `json:"schema"`
	Role   string            `json:"role"`
	Owner  sshAdmissionOwner `json:"owner"`
}

// Only an optional backend capability supplies a copied local snapshot.
type pgSampleWriterGenerationSource interface {
	pgSampleWriterGeneration() pgSampleWriterGeneration
}

// Missing or invalid provenance never changes source validity or cadence.
func pgSampleWriterGenerationFor(backend sshAdmissionBackend) *pgSampleWriterGeneration {
	source, ok := backend.(pgSampleWriterGenerationSource)
	if !ok {
		return nil
	}
	generation := source.pgSampleWriterGeneration()
	if generation.Schema != 1 || !generation.Owner.valid() || (generation.Role != sshAdmissionWatch && generation.Role != sshAdmissionDiagnostic) {
		return nil
	}
	return &generation
}
