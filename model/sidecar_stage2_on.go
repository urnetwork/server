//go:build sidecar_stage2

package model

// §7.4's stage 2 is live in this binary, built once D-SDK-11's desktop app and
// extension releases have shipped: the six routes refuse every client token,
// and enrollment is open (sidecar_preview_model.go).
const sidecarStage2Build = true
