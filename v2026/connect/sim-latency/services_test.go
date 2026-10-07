package main

// Deterministic configuration tests keep measured traffic on the same
// contract-validation path that competition submissions are allowed to edit.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
)

// Verifies the simulation override reaches the real exchange settings instead
// of remaining an unused run-spec value.
func TestSimulationExchangeSettingsExpireIdleForwards(t *testing.T) {
	servicesConfig := DefaultServicesConfig()
	settings := newSimulationExchangeSettings(servicesConfig)
	if settings.ForwardIdleTimeout != 5*time.Second {
		t.Fatalf(
			"forward idle timeout = %s; want %s",
			settings.ForwardIdleTimeout,
			5*time.Second,
		)
	}
}

// Invalid lifetimes fail before listeners or goroutines start.
func TestValidateServicesConfigRejectsNonpositiveForwardIdleTimeout(t *testing.T) {
	for _, forwardIdleTimeout := range []time.Duration{0, -time.Nanosecond} {
		servicesConfig := DefaultServicesConfig()
		servicesConfig.ForwardIdleTimeout = forwardIdleTimeout
		if err := validateServicesConfig(servicesConfig); err == nil {
			t.Errorf("forward idle timeout %s was accepted", forwardIdleTimeout)
		}
	}
}

// Port zero disables packet endpoints; it is not an ephemeral UDP bind.
// H1 is served by the HTTP route, independently of these packet listeners.
func TestSimulationExchangeSettingsUseOnlyH1Listeners(t *testing.T) {
	settings := newSimulationExchangeSettings(DefaultServicesConfig())
	if settings.ListenH3Port != 0 || settings.ListenDnsPort != 0 || len(settings.ListenDnsCompatibilityPorts) != 0 {
		t.Fatalf("simulation inherited production UDP listeners: %+v", settings.ConnectHandlerSettings)
	}
	production := connectserver.DefaultConnectHandlerSettings()
	if production.ListenH3Port == 0 || production.ListenDnsPort == 0 {
		t.Fatal("simulation disabled production listeners")
	}
	if settings.FramerSettings.MaxMessageLen != production.FramerSettings.MaxMessageLen ||
		settings.WriteTimeout != production.WriteTimeout || settings.ReadTimeout != production.ReadTimeout {
		t.Fatal("simulation changed the H1 transport contract")
	}
}

// Explicit ticks and a joined worker prove cancellation cannot start a later
// heartbeat, even if another tick was already queued.
func TestSimulationHandlerHeartbeatsStopBeforeNextRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	self := &Services{cancel: cancel, handlerIds: []server.Id{server.NewId(), server.NewId()}}
	ticks := make(chan time.Time, 1)
	calls := make(chan server.Id, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		self.runHandlerHeartbeats(ctx, ticks, func(_ context.Context, handlerId server.Id) error {
			calls <- handlerId
			return nil
		})
	}()
	ticks <- time.Time{}
	for _, handlerId := range self.handlerIds {
		if got := <-calls; got != handlerId {
			t.Fatalf("heartbeat handler = %s, want %s", got, handlerId)
		}
	}
	cancel()
	ticks <- time.Time{}
	<-done
	if len(calls) != 0 {
		t.Fatal("canceled services refreshed a handler")
	}
}

// Losing a registration cannot silently leave a live socket outside the
// production reliability pool; it stops the services and preserves the cause.
func TestSimulationHandlerHeartbeatFailureStopsServices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	self := &Services{cancel: cancel, handlerIds: []server.Id{server.NewId()}}
	ticks := make(chan time.Time, 1)
	ticks <- time.Time{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		self.runHandlerHeartbeats(ctx, ticks, func(context.Context, server.Id) error {
			return errors.New("synthetic handler registration lost")
		})
	}()
	<-ctx.Done()
	<-done
	if self.runErr == nil || !strings.Contains(self.runErr.Error(), "synthetic handler registration lost") {
		t.Fatalf("heartbeat failure was lost: %v", self.runErr)
	}
}
