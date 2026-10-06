//go:build !linux

package server

import "context"

// The deployed capture transport requires Linux protected Unix sockets and
// descriptor-backed mappings. Other targets remain disabled, never insecure.
func LoadArinShadowRuntimeConfig() (*ArinShadowRuntimeConfig, error) { return nil, nil }
func StartArinShadowRuntime(ctx context.Context, config *ArinShadowRuntimeConfig, role string, factory func(context.Context) (ArinShadowRPCHandler, func(), error)) (*ArinShadowRuntime, error) {
	if config != nil {
		return nil, ErrArinShadowInput
	}
	return nil, nil
}
