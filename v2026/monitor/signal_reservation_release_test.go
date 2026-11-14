package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Incident B replayed from recorded Mimir history: the proxy's
// release-pending errors appear at 14:46Z on 10-26 (23/s, then 409/s) and
// hold above 10/s for five evaluations at 14:50Z, seven minutes after onset;
// the error child did not exist before 14:46Z and reads as zero there. The
// healthy reference raises nothing. No recorded history carries the deferred
// owner's dropped and expired results yet.
func TestReservationReleaseReplaysIncidentB(t *testing.T) {
	cases := loadStormReplayFixture(t, "reservation-release")
	for _, test := range []struct {
		name    string
		targets []string
	}{
		{name: "incident-b", targets: []string{"main/proxy"}},
		{name: "incident-b-before"},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewReservationReleaseSignal(), cases[test.name], nil)
		want := map[string][]string{}
		if 0 < len(test.targets) {
			want["reservation-release-errors"] = test.targets
		}
		requireStormAlerts(t, test.name, alerts, want)
	}
	alert := requireAlertClass(t, runStormReplay(t, NewReservationReleaseSignal(), cases["incident-b"], nil), "reservation-release-errors")
	if alert.Severity != SeverityWarn || alert.SignalNumber != "26.6" || !strings.Contains(alert.Observed, "release_pending_errors_per_second_now=2136.7") {
		t.Fatalf("%+v", alert)
	}
}

// A synthetic answer: per service, release-pending errors (NaN for a missing
// error child), dropped and expired compensations, and how many series of
// each operation it publishes at each step (NaN for none).
type reservationReleaseService struct {
	errors           []float64
	dropped, expired []float64
	publishers       []float64
}

var reservationReleaseEveryStep = stormRepeat(20, 10)

func reservationReleaseResponse(t *testing.T, services map[string]reservationReleaseService) string {
	series := []stormSyntheticSeries{}
	for name, service := range services {
		labels := map[string]string{"service": name}
		if service.errors != nil {
			series = append(series, stormSyntheticSeries{part: "release_errors", labels: labels, values: service.errors})
		}
		for result, values := range map[string][]float64{"dropped": service.dropped, "expired": service.expired, "completed": stormRepeat(40, 10)} {
			if values != nil {
				series = append(series, stormSyntheticSeries{part: "compensation", labels: map[string]string{"service": name, "result": result}, values: values})
			}
		}
		if service.publishers != nil {
			series = append(series,
				stormSyntheticSeries{part: "release_publishers", labels: labels, values: service.publishers},
				stormSyntheticSeries{part: "compensation_publishers", labels: labels, values: service.publishers},
			)
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(10), time.Minute, series)
}

// A publisher without a result child has had none; the compensation arm is
// dropped plus expired above 1/s for ten minutes; the api and the proxy are
// required, other services are judged while they publish.
func TestReservationReleaseArmsAndPublishers(t *testing.T) {
	probe := reservationReleaseProbe{}
	release, compensation := "reservation-release-errors main/proxy", "compensation-dropped main/proxy"
	visibility := "reservation-release-unobservable main/reservation-release"
	quiet := func() map[string]reservationReleaseService {
		return map[string]reservationReleaseService{
			"api":   {publishers: reservationReleaseEveryStep},
			"proxy": {errors: stormRepeat(0.6, 10), publishers: reservationReleaseEveryStep},
		}
	}
	errorsAbove, errorsAtLevel, lost, lostAtLevel, lostBrief, taskworker, silentProxy := quiet(), quiet(), quiet(), quiet(), quiet(), quiet(), quiet()
	errorsAbove["proxy"] = reservationReleaseService{errors: stormRepeat(23, 10), publishers: reservationReleaseEveryStep}
	errorsAtLevel["proxy"] = reservationReleaseService{errors: stormRepeat(10, 10), publishers: reservationReleaseEveryStep}
	lost["proxy"] = reservationReleaseService{dropped: stormRepeat(0.7, 10), expired: stormRepeat(0.4, 10), publishers: reservationReleaseEveryStep}
	lostAtLevel["proxy"] = reservationReleaseService{dropped: stormRepeat(0.5, 10), expired: stormRepeat(0.5, 10), publishers: reservationReleaseEveryStep}
	lostBrief["proxy"] = reservationReleaseService{dropped: append([]float64{0.2}, stormRepeat(2, 9)...), publishers: reservationReleaseEveryStep}
	taskworker["taskworker"] = reservationReleaseService{errors: append(stormRepeat(math.NaN(), 5), stormRepeat(30, 5)...), publishers: append(stormRepeat(math.NaN(), 5), stormRepeat(1, 5)...)}
	silentProxy["proxy"] = reservationReleaseService{}
	for _, test := range []struct {
		name     string
		services map[string]reservationReleaseService
		want     map[string]string
	}{
		{name: "healthy", services: quiet(), want: map[string]string{release: "healthy", compensation: "healthy", "reservation-release-errors main/api": "healthy", visibility: "healthy"}},
		{name: "release errors above", services: errorsAbove, want: map[string]string{release: "warn", compensation: "healthy"}},
		{name: "release errors at the level", services: errorsAtLevel, want: map[string]string{release: "healthy"}},
		{name: "dropped plus expired above", services: lost, want: map[string]string{compensation: "warn", release: "healthy"}},
		{name: "dropped plus expired at the level", services: lostAtLevel, want: map[string]string{compensation: "healthy"}},
		{name: "dropped for nine minutes", services: lostBrief, want: map[string]string{compensation: "hold"}},
		{name: "an optional publisher is judged while it publishes", services: taskworker, want: map[string]string{"reservation-release-errors main/taskworker": "warn", "compensation-dropped main/taskworker": "hold", visibility: "healthy"}},
		{name: "a required publisher that stops publishing", services: silentProxy, want: map[string]string{release: "hold", compensation: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, reservationReleaseResponse(t, test.services), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestReservationReleaseQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultReservationReleaseSettings()
	if settings.ReleaseErrors != 10 || settings.ReleaseHold != 5*time.Minute || settings.CompensationLost != 1 || settings.CompensationHold != 10*time.Minute {
		t.Fatalf("§26.6 policy changed: %+v", settings)
	}
	query := reservationReleaseQuery("main", settings)
	for _, want := range []string{
		`sum by (service) (rate(urnetwork_redis_contract_reservation_total{env="main",operation="release-pending",result="error"}[5m]))`,
		`sum by (service, result) (rate(urnetwork_redis_contract_reservation_total{env="main",operation="compensation"}[5m]))`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost the catalog expression %s", want)
		}
	}
}
