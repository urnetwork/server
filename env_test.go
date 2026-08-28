package server

import (
	"testing"
)

func TestLocalEvaluationCredentialRequiresExplicitLocalMode(t *testing.T) {
	t.Setenv("APEX_CONTAINER_EVALUATION", "")
	t.Setenv("EVALUATION_DB_PASSWORD", "override")
	if got := localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured"); got != "configured" {
		t.Fatalf("ordinary environment used evaluator credential %q", got)
	}

	t.Setenv("APEX_CONTAINER_EVALUATION", "true")
	t.Setenv("WARP_ENV", "local")
	if got := localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured"); got != "override" {
		t.Fatalf("evaluator credential = %q, want override", got)
	}

	t.Setenv("WARP_ENV", "main")
	assertPanics(t, func() {
		localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured")
	})
	t.Setenv("WARP_ENV", "local")
	t.Setenv("EVALUATION_DB_PASSWORD", "")
	assertPanics(t, func() {
		localEvaluationCredential("EVALUATION_DB_PASSWORD", "configured")
	})
}

func assertPanics(t *testing.T, run func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	run()
}
