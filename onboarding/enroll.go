package onboarding

import "time"

// EnrollmentHorizon bounds enrollment on the device path: a network older
// than this that registers its first device is not a new sign-up but an old
// account (or one created by automation long before its first real use) and
// does not enter the campaign with today's cohort.
const EnrollmentHorizon = 30 * 24 * time.Hour

// Enroll decides whether a network enters the campaign now (decision
// 2026-09-10, mmm/onboarding/RUN-MAIN.md option 2). Two entry points call it:
//
//   - the account path (network create, or auth verify when the address had to
//     be verified), with viaDevice false: a network enters when its login is an
//     email address; one without leaves the decision to the device path;
//   - the device path (the network's first device client), with viaDevice
//     true: a network without an email login enters, as long as it is not
//     older than EnrollmentHorizon; one with an email login is the account
//     path's (it may still be waiting for its verification).
//
// A network with neither an email login nor a device never enters: on Main
// about 85% of new networks are created through the API and never register a
// device, and they cannot see an offer screen or be mailed. The cohort time is
// the enrollment time.
func Enroll(hasEmailLogin bool, viaDevice bool, createdAt time.Time, now time.Time) bool {
	if !viaDevice {
		return hasEmailLogin
	}
	if hasEmailLogin {
		return false
	}
	return !createdAt.Before(now.Add(-EnrollmentHorizon))
}
