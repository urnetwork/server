package handlers

import "testing"

func TestPayoutArtifactHistoryPrefixScopesExactEpochAndOperator(t *testing.T) {
	got, err := payoutArtifactHistoryPrefix("blob/operator-1", "ur-subnet-testnet-v1", "521", "0", "2")
	if err != nil {
		t.Fatal(err)
	}
	want := "blob/operator-1/st/v1/history/ur-subnet-testnet-v1/521/0/2/"
	if got != want {
		t.Fatalf("history prefix = %q, want %q", got, want)
	}
	all, err := payoutArtifactHistoryPrefix("blob/operator-1", "ur-subnet-testnet-v1", "521", "", "")
	if err != nil || all != "blob/operator-1/st/v1/history/ur-subnet-testnet-v1/521/" {
		t.Fatalf("deployment history prefix = %q, %v", all, err)
	}
}

func TestPayoutArtifactHistoryPrefixRejectsAmbiguousAndUnsafeFilters(t *testing.T) {
	tests := []struct {
		deployment string
		netuid     string
		epoch      string
		noID       string
	}{
		{deployment: "../other", netuid: "521"},
		{deployment: "release", netuid: "not-a-number"},
		{deployment: "release", netuid: "0"},
		{deployment: "release", netuid: "521", noID: "1"},
		{deployment: "release", netuid: "521", epoch: "x"},
		{deployment: "release", netuid: "521", epoch: "0", noID: "0"},
	}
	for _, test := range tests {
		if got, err := payoutArtifactHistoryPrefix("blob", test.deployment, test.netuid, test.epoch, test.noID); err == nil {
			t.Errorf("unsafe history filter produced %q for %+v", got, test)
		}
	}
}
