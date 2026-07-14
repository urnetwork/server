package server

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestWarmup(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, Warmup: false}).Run(t, func(t testing.TB) {

		aRun := false
		a := func() {
			aRun = true
		}
		bRun := false
		b := func() {
			bRun = true
		}
		cRun := false
		c := func() {
			cRun = true
		}

		OnWarmup(a)
		connect.AssertEqual(t, aRun, false)
		OnWarmup(b)
		connect.AssertEqual(t, bRun, false)
		Warmup()
		connect.AssertEqual(t, aRun, true)
		connect.AssertEqual(t, bRun, true)
		connect.AssertEqual(t, cRun, false)
		OnWarmup(c)
		connect.AssertEqual(t, cRun, true)
	})
}
