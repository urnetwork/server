package model

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestPasswordRotationCutoffIsSampledAfterLifecycleAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		network, user := server.NewId(), server.NewId()
		userAuth := Testing_CreateNetwork(ctx, network, "reset-lock", user)
		actor := session.Testing_CreateClientSession(ctx, session.NewByJwt(network, user, "reset-lock", false, false))
		defer actor.Cancel()
		reset, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, actor)
		if err != nil || reset.ResetCode == nil {
			t.Fatal(reset, err)
		}
		admitted, resume := make(chan struct{}), make(chan struct{})
		var once sync.Once
		pausedCtx := context.WithValue(ctx, authPasswordSetBeforeLifecycleLockKey{}, func() { once.Do(func() { close(admitted); <-resume }) })
		rotator := session.Testing_CreateClientSession(pausedCtx, actor.ByJwt)
		defer rotator.Cancel()
		done := make(chan error, 1)
		go server.HandleError(func() {
			result, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *reset.ResetCode, Password: "new-session-password"}, rotator)
			if err == nil && (result == nil || result.Error != nil) {
				err = errors.New("rotation failed")
			}
			done <- err
		}, func(err error) { done <- err })
		select {
		case <-admitted:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// This is a successful old-password login admitted before the reset's
		// exclusive lifecycle lock. A cutoff sampled before the wait spared it.
		before := session.NewByJwt(network, user, "reset-lock", false, false)
		if _, err := session.MintNetworkSession(ctx, before, "password"); err != nil {
			close(resume)
			t.Fatal(err)
		}
		close(resume)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if !errors.Is(session.ValidateByJwtState(ctx, before, false), session.ErrByJwtInactive) {
			t.Fatal("rotation spared a login admitted before its exclusive cutoff")
		}
		after := session.NewByJwt(network, user, "reset-lock", false, false)
		if _, err := session.MintNetworkSession(ctx, after, "password"); err != nil {
			t.Fatal("post-rotation login failed", err)
		}
		if err := RecoverSessionOperations(ctx, 32); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(session.CheckSession(ctx, network, *before.SessionId), session.ErrSessionRevoked) {
			t.Fatal("old session escaped durable retirement")
		}
		if err := session.ValidateByJwtState(ctx, after, false); err != nil {
			t.Fatal("old cleanup retired fresh login", err)
		}
	})
}
