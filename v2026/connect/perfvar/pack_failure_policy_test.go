// This file pins which measured Pack failures are evidence of data loss and
// which have an exact enclosing recovery owner.
package perfvar

import (
	"strings"
	"testing"
)

// The measured-interval gate exempts only failures whose exact Pack caller
// proved that upstream TCP state retains or can regenerate them. Every other
// data failure remains fatal evidence.
func TestValidateMeasuredPackFailuresAllowsOnlyProviderRecoverable(t *testing.T) {
	floor := perfvarPackFailureCounts{
		deviceFailureCount:              1,
		providerFailureCount:            2,
		providerRecoverableFailureCount: 1,
	}
	for _, testCase := range []struct {
		name                   string
		end                    perfvarPackFailureCounts
		allowProviderDatagrams bool
		wantErrorPart          string
	}{
		{
			name: "no measured failures",
			end:  floor,
		},
		{
			name: "provider failures all recoverable",
			end: perfvarPackFailureCounts{
				deviceFailureCount:              1,
				providerFailureCount:            5,
				providerRecoverableFailureCount: 4,
			},
		},
		{
			name: "provider unclassified failure",
			end: perfvarPackFailureCounts{
				deviceFailureCount:              1,
				providerFailureCount:            5,
				providerRecoverableFailureCount: 3,
			},
			wantErrorPart: "provider-unrecoverable=1",
		},
		{
			name: "provider datagram outside measured probe scope",
			end: perfvarPackFailureCounts{
				deviceFailureCount:              1,
				providerFailureCount:            3,
				providerRecoverableFailureCount: 1,
				providerDatagramFailureCount:    1,
			},
			wantErrorPart: "provider-unrecoverable=1",
		},
		{
			name: "provider datagram inside measured probe scope",
			end: perfvarPackFailureCounts{
				deviceFailureCount:              1,
				providerFailureCount:            3,
				providerRecoverableFailureCount: 1,
				providerDatagramFailureCount:    1,
			},
			allowProviderDatagrams: true,
		},
		{
			name: "device failure",
			end: perfvarPackFailureCounts{
				deviceFailureCount:              2,
				providerFailureCount:            3,
				providerRecoverableFailureCount: 2,
			},
			wantErrorPart: "device=1",
		},
		{
			name: "recoverable counter exceeds all failures",
			end: perfvarPackFailureCounts{
				deviceFailureCount:              1,
				providerFailureCount:            3,
				providerRecoverableFailureCount: 4,
			},
			wantErrorPart: "exceeded all failures",
		},
		{
			name: "recoverable counter moved backward",
			end: perfvarPackFailureCounts{
				deviceFailureCount:              1,
				providerFailureCount:            2,
				providerRecoverableFailureCount: 0,
			},
			wantErrorPart: "moved backward",
		},
	} {
		failureFloor := floor
		path := &fullTunPath{
			activePackFailureFloor:            &failureFloor,
			allowProviderDatagramPackFailures: testCase.allowProviderDatagrams,
		}
		err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{
			packFailures: testCase.end,
		})
		if testCase.wantErrorPart == "" {
			if err != nil {
				t.Fatalf("%s: validate recoverable failures: %v", testCase.name, err)
			}
			if path.activePackFailureFloor == nil ||
				*path.activePackFailureFloor != testCase.end {
				t.Fatalf(
					"%s: successful validation floor=%+v, want %+v",
					testCase.name,
					path.activePackFailureFloor,
					testCase.end,
				)
			}
			if testCase.allowProviderDatagrams {
				path.setAllowProviderDatagramPackFailures(false)
				if err := path.validateMeasuredPackFailures(perfvarCarrierBoundary{
					packFailures: testCase.end,
				}); err != nil {
					t.Fatalf(
						"%s: revalidated accounted datagram failure: %v",
						testCase.name,
						err,
					)
				}
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), testCase.wantErrorPart) {
			t.Fatalf(
				"%s: validation error=%v, want substring %q",
				testCase.name,
				err,
				testCase.wantErrorPart,
			)
		}
	}
}
