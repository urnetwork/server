package server

import (
	"context"
	"io"
	"path/filepath"
	"time"
)

// Resource/inventory setup precedes the independently bounded ninety-second
// cohort lease. A reused host bridge must survive both finite phases.
const ArinShadowRPCBridgeMaxAge = 3 * time.Minute

// ServeArinShadowRPCBridge routes only to the exact protected host inventory.
// No socket is learned from a request. The bridge does not read a credential
// or interpret classification data; each endpoint verifies the signed packet.
func ServeArinShadowRPCBridge(ctx context.Context, input io.Reader, output io.Writer, sockets map[Id]string, direct string) error {
	if ctx == nil || ctx.Err() != nil || len(sockets) > 64 || (direct == "" && len(sockets) == 0) || (direct != "" && len(sockets) != 0) {
		return ErrArinShadowInput
	}
	validPath := func(path string) bool { return filepath.IsAbs(path) && filepath.Clean(path) == path }
	if direct != "" && !validPath(direct) {
		return ErrArinShadowInput
	}
	for id, path := range sockets {
		if id == (Id{}) || !validPath(path) {
			return ErrArinShadowInput
		}
	}
	bounded, cancel := context.WithTimeout(ctx, ArinShadowRPCBridgeMaxAge)
	defer cancel()
	stopIO := context.AfterFunc(bounded, func() {
		if c, ok := input.(io.Closer); ok {
			c.Close()
		}
		if c, ok := output.(io.Closer); ok {
			c.Close()
		}
	})
	defer stopIO()
	for calls := 0; calls < ArinShadowRPCMaxCalls && bounded.Err() == nil; calls++ {
		request, err := ReadArinShadowRPCFrame(input, ArinShadowRPCRequestLimit)
		if err != nil {
			return ErrArinShadowInput
		}
		destination := direct
		if destination == "" {
			nonce, err := ArinShadowRPCDestination(request)
			if err != nil {
				return err
			}
			destination = sockets[nonce]
			if destination == "" {
				return ErrArinShadowInput
			}
		}
		call, stop := context.WithTimeout(bounded, ArinShadowRPCCallTimeout)
		reply, err := ArinShadowUnixRoundTrip(destination)(call, request)
		stop()
		if err != nil || WriteArinShadowRPCFrame(output, reply, ArinShadowRPCResponseLimit) != nil {
			return ErrArinShadowInput
		}
	}
	return ErrArinShadowInput
}
