// Replay expiry arithmetic retains the original exclusive terminal horizon.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Exact before/at/after boundaries need no waiting or replacement process clock.
func TestRegistrationSessionHorizonIsExclusiveAndNeverRenewed(t *testing.T) {
	deadline := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	for _, step := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before", deadline.Add(-time.Nanosecond), true},
		{"at", deadline, false},
		{"after", deadline.Add(time.Nanosecond), false},
	} {
		t.Run(step.name, func(t *testing.T) {
			credential := session.NewByJwt(server.NewId(), server.NewId(), "synthetic horizon", false, false)
			created := credential.CreateTime
			if got := restoreRegistrationHorizon(credential, &deadline, step.now); got != step.want {
				t.Fatalf("original horizon accepted=%t want=%t", got, step.want)
			}
			if step.want && (!credential.AcceptUntil().Equal(deadline) || !credential.CreateTime.Equal(created)) {
				t.Fatal("replay extended the original horizon or changed lineage")
			}
		})
	}
}
