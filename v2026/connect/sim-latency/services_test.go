package main

// Deterministic configuration tests keep measured traffic on the same
// contract-validation path that competition submissions are allowed to edit.

import (
	"testing"
	"time"
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
