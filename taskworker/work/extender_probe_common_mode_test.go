package work

// The verdict of one family's pass (extender_probe_common_mode.go): a prober
// fault is held, and every outage the extenders themselves can have is
// counted, however large one operator is.

import (
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A family's results: for each network, its addresses and how many failed.
func testJudgedResults(networks ...[2]int) []extenderProbeJudgedResult {
	results := []extenderProbeJudgedResult{}
	for _, network := range networks {
		networkId := server.NewId()
		addressCount, failedCount := network[0], network[1]
		for i := range addressCount {
			results = append(results, extenderProbeJudgedResult{
				networkId: networkId,
				failed:    i < failedCount,
			})
		}
	}
	return results
}

// Main's open directory on 11-11 (research extender-health-2026-11-11 §1.1):
// four networks hold 83, 63, 25 and 16 extenders, four more 6, 3, 2 and 2,
// and eight one each.
var testJudgedMainNetworkSizes = []int{83, 63, 25, 16, 6, 3, 2, 2, 1, 1, 1, 1, 1, 1, 1, 1}

// Main's directory with the given networks, by index, failed outright and the
// rest answering.
func testJudgedMain(failedNetworks ...int) []extenderProbeJudgedResult {
	networks := [][2]int{}
	for i, size := range testJudgedMainNetworkSizes {
		failed := 0
		for _, failedNetwork := range failedNetworks {
			if failedNetwork == i {
				failed = size
			}
		}
		networks = append(networks, [2]int{size, failed})
	}
	return testJudgedResults(networks...)
}

func TestExtenderProbeVerdictHoldsAProberFault(t *testing.T) {
	// every network, every address
	all := []int{}
	for i := range testJudgedMainNetworkSizes {
		all = append(all, i)
	}
	judgement := judgeExtenderProbeFamily(4, testJudgedMain(all...), false)
	connect.AssertEqual(t, judgement.verdict, extenderProbeVerdictCommonMode)
	connect.AssertEqual(t, judgement.addressCount, 208)
	connect.AssertEqual(t, judgement.failedAddressCount, 208)
	connect.AssertEqual(t, judgement.networkCount, 16)
	connect.AssertEqual(t, judgement.failedNetworkCount, 16)
	connect.AssertEqual(t, judgement.held(), true)

	// a prober losing four dials in five, everywhere
	networks := [][2]int{}
	for _, size := range testJudgedMainNetworkSizes {
		networks = append(networks, [2]int{size, (size*4 + 4) / 5})
	}
	connect.AssertEqual(t, judgeExtenderProbeFamily(4, testJudgedResults(networks...), false).verdict, extenderProbeVerdictCommonMode)
}

func TestExtenderProbeVerdictCountsTheExtendersOwnOutages(t *testing.T) {
	for _, test := range []struct {
		name           string
		failedNetworks []int
	}{
		{"a healthy pass", nil},
		{"the largest operator down", []int{0}},
		{"the two largest operators down, 70% of the addresses", []int{0, 1}},
		{"the four largest operators down, 90% of the addresses", []int{0, 1, 2, 3}},
		{"every small operator down", []int{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}},
		{"the largest two and half the small ones down", []int{0, 1, 8, 9, 10, 11, 12, 13}},
	} {
		judgement := judgeExtenderProbeFamily(4, testJudgedMain(test.failedNetworks...), false)
		if judgement.held() {
			t.Errorf("%s was held (%s): %d of %d addresses across %d of %d networks", test.name, judgement.verdict, judgement.failedAddressCount, judgement.addressCount, judgement.failedNetworkCount, judgement.networkCount)
		}
	}
}

// The spread decides only with enough networks and addresses to say anything.
func TestExtenderProbeVerdictNeedsEnoughNetworksAndAddresses(t *testing.T) {
	// three networks failing outright: a statement about three operators
	threeNetworks := testJudgedResults([2]int{4, 4}, [2]int{4, 4}, [2]int{4, 4})
	connect.AssertEqual(t, judgeExtenderProbeFamily(6, threeNetworks, false).verdict, extenderProbeVerdictCounted)

	// four networks, seven addresses
	sevenAddresses := testJudgedResults([2]int{4, 4}, [2]int{1, 1}, [2]int{1, 1}, [2]int{1, 1})
	connect.AssertEqual(t, judgeExtenderProbeFamily(6, sevenAddresses, false).verdict, extenderProbeVerdictCounted)

	// four networks, eight addresses: judged
	eightAddresses := testJudgedResults([2]int{5, 5}, [2]int{1, 1}, [2]int{1, 1}, [2]int{1, 1})
	connect.AssertEqual(t, judgeExtenderProbeFamily(6, eightAddresses, false).verdict, extenderProbeVerdictCommonMode)
}

// Three quarters of the addresses, and three quarters of the networks, each
// losing half or more of their own.
func TestExtenderProbeVerdictThresholds(t *testing.T) {
	// 6 of 8 addresses, 3 of 4 networks: held
	connect.AssertEqual(t,
		judgeExtenderProbeFamily(4, testJudgedResults([2]int{2, 2}, [2]int{2, 2}, [2]int{2, 2}, [2]int{2, 0}), false).verdict,
		extenderProbeVerdictCommonMode,
	)
	// 5 of 8 addresses: counted
	connect.AssertEqual(t,
		judgeExtenderProbeFamily(4, testJudgedResults([2]int{2, 2}, [2]int{2, 2}, [2]int{2, 1}, [2]int{2, 0}), false).verdict,
		extenderProbeVerdictCounted,
	)
	// a network that lost half its addresses failed: 9 of 12 addresses, 4 of
	// 4 networks
	connect.AssertEqual(t,
		judgeExtenderProbeFamily(4, testJudgedResults([2]int{3, 3}, [2]int{3, 2}, [2]int{3, 2}, [2]int{3, 2}), false).verdict,
		extenderProbeVerdictCommonMode,
	)
	// 14 of 16 addresses on 2 of 4 networks: one operator's fleet, counted
	connect.AssertEqual(t,
		judgeExtenderProbeFamily(4, testJudgedResults([2]int{13, 13}, [2]int{1, 0}, [2]int{1, 1}, [2]int{1, 0}), false).verdict,
		extenderProbeVerdictCounted,
	)
}

// The control target judges a family too small for the spread, but only with
// most of the family failing: a control that fails while the extenders answer
// is the control's fault, and a dead extender among working ones still counts.
func TestExtenderProbeVerdictOfAFailedControl(t *testing.T) {
	// two extenders, both failing, the control with them
	connect.AssertEqual(t,
		judgeExtenderProbeFamily(4, testJudgedResults([2]int{1, 1}, [2]int{1, 1}), true).verdict,
		extenderProbeVerdictControlFailed,
	)
	// the same without the control: counted
	connect.AssertEqual(t,
		judgeExtenderProbeFamily(4, testJudgedResults([2]int{1, 1}, [2]int{1, 1}), false).verdict,
		extenderProbeVerdictCounted,
	)
	// one dead extender of four, with the control failing: counted
	connect.AssertEqual(t,
		judgeExtenderProbeFamily(4, testJudgedResults([2]int{1, 1}, [2]int{1, 0}, [2]int{1, 0}, [2]int{1, 0}), true).verdict,
		extenderProbeVerdictCounted,
	)
	// nothing failed: counted, whatever the control said
	connect.AssertEqual(t,
		judgeExtenderProbeFamily(4, testJudgedResults([2]int{2, 0}), true).verdict,
		extenderProbeVerdictCounted,
	)
	// no address answered at all: counted
	connect.AssertEqual(t, judgeExtenderProbeFamily(4, nil, true).verdict, extenderProbeVerdictCounted)
	// a failed control and a spread together read as the control
	all := []int{}
	for i := range testJudgedMainNetworkSizes {
		all = append(all, i)
	}
	connect.AssertEqual(t, judgeExtenderProbeFamily(4, testJudgedMain(all...), true).verdict, extenderProbeVerdictControlFailed)
}
