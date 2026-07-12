package controller

import (
	"testing"

	"github.com/urnetwork/server/model"
)

// skipWithoutProYml skips a test that asserts the CONFIGURED product spec (prices,
// grants, data-code skus from pro.yml) when this environment has no pro.yml. The
// absent contract -- purchases refused, grants noop, x402 catalog empty -- is owned
// by model.TestProAbsent and TestX402BlankConfigNoops.
func skipWithoutProYml(t testing.TB) {
	if model.Pro().MaxConcurrentClients(true) == 0 {
		t.Skip("pro.yml is not present in this environment; see model.TestProAbsent")
	}
}
