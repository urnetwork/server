//go:build !sidecar_stage2

package model

// §7.4's stage 2 is not live in this binary: the six routes keep the app admin
// class, and enrollment refuses strict_exp clients outside preview_networks
// (sidecar_preview_model.go).
const sidecarStage2Build = false
