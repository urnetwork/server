// The shared machine formatter retains legacy states and refuses to invent
// database observations from a generic integration or caller error.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/controller"
)

// Both --once and continuous supervision use this same serialization seam;
// worker/broker stdout remains a separate bounded child-output protocol.
func TestSourceSupervisorMachineReportDoesNotInventDatabaseProof(t *testing.T) {
	content, err := controller.MarshalCompetitionSupervisorReport("lease_lost", context.DeadlineExceeded)
	if err != nil || string(content) != `{"state":"lease_lost","error":"context deadline exceeded"}` || strings.Contains(string(content), "renewal_database") {
		t.Fatal("generic supervisor error acquired database evidence or changed legacy output", string(content), err)
	}
}
