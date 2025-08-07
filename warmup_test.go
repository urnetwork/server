package server

import (
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestWarmup(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {

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

		Warm(a)
		assert.Equal(t, aRun, false)
		Warm(b)
		assert.Equal(t, bRun, false)
		Warmup()
		assert.Equal(t, aRun, true)
		assert.Equal(t, bRun, true)
		assert.Equal(t, cRun, false)
		Warm(c)
		assert.Equal(t, cRun, true)
	})
}
