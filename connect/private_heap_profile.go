package connect

import (
	"context"
	"errors"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/internal/privateheapprofile"
)

// Only the exact startup target owns SDK payload accounting. The profile flag
// may be present in a shared image; unrelated instances keep the nil fast path.
func privateHeapProfileTargetsThisInstance(target string) bool {
	if target == "" || !privateheapprofile.ValidTarget(target) {
		return false
	}
	host, err := server.Host()
	if err != nil {
		return false
	}
	block, err := server.Block()
	return err == nil && target == host+"/"+block
}

func startPrivateHeapProfile(ctx context.Context, target string, exchange *Exchange, handler *ConnectHandler) (*privateheapprofile.Server, error) {
	if target == "" {
		return nil, nil
	}
	if !privateheapprofile.ValidTarget(target) {
		return nil, errors.New("private_heap_target_invalid")
	}
	host, err := server.Host()
	if err != nil {
		return nil, err
	}
	block, err := server.Block()
	if err != nil {
		return nil, err
	}
	targetHost, targetBlock, _ := strings.Cut(target, "/")
	return privateheapprofile.Start(ctx, privateheapprofile.Config{
		TargetHost: targetHost, TargetBlock: targetBlock, Host: host, Block: block,
		Companion: func() privateheapprofile.Companion { return privateHeapProfileCompanion(exchange, handler) },
	})
}
