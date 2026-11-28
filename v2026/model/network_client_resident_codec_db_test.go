package model

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The resident record's nomination and removal paths with gob and typed
// records side by side (CPUOPT7.md §12): each compares the exact bytes it
// read, so a record of either format is replaced or removed only while it is
// the record that was read.

func residentCodecDbTestNomination(clientId server.Id, instanceId server.Id) *NetworkClientResident {
	return &NetworkClientResident{
		ClientId:              clientId,
		InstanceId:            instanceId,
		ResidentId:            server.NewId(),
		ResidentHost:          "host-a.example",
		ResidentService:       "connect",
		ResidentBlock:         "block-a",
		ResidentInternalPorts: []int{7001, 7002},
	}
}

func residentCodecDbTestFormats() []NetworkClientResidentFormat {
	return []NetworkClientResidentFormat{NetworkClientResidentFormatGob, NetworkClientResidentFormatTyped}
}

// With the flag off a nomination writes the gob record an old reader reads;
// with it on, the typed record. Every reader reads both.
func TestNominateResidentWithFormatWritesTheFormat(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, format := range residentCodecDbTestFormats() {
			nomination := residentCodecDbTestNomination(server.NewId(), server.NewId())
			if !NominateResidentWithFormat(ctx, nil, nomination, time.Minute, format) {
				t.Fatalf("%s: not nominated", format)
			}
			record := Testing_ResidentRecord(ctx, nomination.ClientId)
			if isTypedNetworkClientResident(record) != (format == NetworkClientResidentFormatTyped) {
				t.Fatalf("%s: the record starts %#x", format, record[0])
			}
			if _, err := networkClientResidentOldReaderDecode(record); (err == nil) != (format == NetworkClientResidentFormatGob) {
				t.Fatalf("%s: an old reader's decode = %v", format, err)
			}
			resident := GetResidentForClient(ctx, nomination.ClientId, 0)
			if resident == nil {
				t.Fatalf("%s: no resident", format)
			}
			assertNetworkClientResidentFieldsEqual(t, nomination, resident)
			if resident := GetResidentForClientWithInstance(ctx, nomination.ClientId, nomination.InstanceId, time.Minute); resident == nil || resident.ResidentId != nomination.ResidentId {
				t.Fatalf("%s: the instance's resident is not read", format)
			}
		}
	})
}

// A nomination replaces a record of the other format by its exact bytes, as
// it replaces one of its own: another instance replaces it, the same instance
// only in place of the resident it names.
func TestNominateResidentReplacesAcrossFormats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, existingFormat := range residentCodecDbTestFormats() {
			for _, nominationFormat := range residentCodecDbTestFormats() {
				clientId, instanceId := server.NewId(), server.NewId()
				existing := residentCodecDbTestNomination(clientId, instanceId)
				if !NominateResidentWithFormat(ctx, nil, existing, time.Minute, existingFormat) {
					t.Fatal("the existing resident was not nominated")
				}
				existingRecord := Testing_ResidentRecord(ctx, clientId)

				// the same instance, naming another resident: kept
				otherResidentId := server.NewId()
				sameInstance := residentCodecDbTestNomination(clientId, instanceId)
				if NominateResidentWithFormat(ctx, &otherResidentId, sameInstance, time.Minute, nominationFormat) {
					t.Fatalf("%s over %s: replaced a resident it did not name", nominationFormat, existingFormat)
				}
				if NominateResidentWithFormat(ctx, nil, sameInstance, time.Minute, nominationFormat) {
					t.Fatalf("%s over %s: the same instance replaced its resident without naming it", nominationFormat, existingFormat)
				}
				if !bytes.Equal(Testing_ResidentRecord(ctx, clientId), existingRecord) {
					t.Fatalf("%s over %s: a refused nomination changed the record", nominationFormat, existingFormat)
				}

				// the same instance, naming the resident: replaced
				if !NominateResidentWithFormat(ctx, &existing.ResidentId, sameInstance, time.Minute, nominationFormat) {
					t.Fatalf("%s over %s: the named resident was not replaced", nominationFormat, existingFormat)
				}
				record := Testing_ResidentRecord(ctx, clientId)
				if isTypedNetworkClientResident(record) != (nominationFormat == NetworkClientResidentFormatTyped) {
					t.Fatalf("%s over %s: the replacement starts %#x", nominationFormat, existingFormat, record[0])
				}
				assertNetworkClientResidentFieldsEqual(t, sameInstance, GetResidentForClient(ctx, clientId, 0))

				// another instance: replaced
				otherInstance := residentCodecDbTestNomination(clientId, server.NewId())
				if !NominateResidentWithFormat(ctx, nil, otherInstance, time.Minute, existingFormat) {
					t.Fatalf("%s over %s: another instance did not replace the resident", existingFormat, nominationFormat)
				}
				assertNetworkClientResidentFieldsEqual(t, otherInstance, GetResidentForClient(ctx, clientId, 0))
			}
		}
	})
}

// Nominations of both formats racing for one client agree on one resident,
// as nominations of one format do (TestNominateResident).
func TestNominateResidentMixedFormatsAgree(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		clientId := server.NewId()
		var residentIdToReplace *server.Id
		for round := range 8 {
			instanceId := server.NewId()
			var stateLock sync.Mutex
			residents := []NetworkClientResident{}
			var wg sync.WaitGroup
			for i := range 64 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					format := NetworkClientResidentFormatGob
					if i%2 == 1 {
						format = NetworkClientResidentFormatTyped
					}
					nomination := residentCodecDbTestNomination(clientId, instanceId)
					resident := nomination
					if !NominateResidentWithFormat(ctx, residentIdToReplace, nomination, time.Minute, format) {
						resident = GetResidentForClient(ctx, clientId, time.Minute)
					}
					if resident == nil {
						t.Error("no resident after a refused nomination")
						return
					}
					func() {
						stateLock.Lock()
						defer stateLock.Unlock()
						residents = append(residents, *resident)
					}()
				}()
			}
			wg.Wait()
			if len(residents) != 64 {
				t.Fatalf("round %d: %d residents", round, len(residents))
			}
			for _, resident := range residents[1:] {
				assertNetworkClientResidentFieldsEqual(t, &residents[0], &resident)
			}
			if residentIdToReplace != nil && residents[0].ResidentId == *residentIdToReplace {
				t.Fatalf("round %d kept the replaced resident", round)
			}
			residentId := residents[0].ResidentId
			residentIdToReplace = &residentId
		}
	})
}

// A record no reader of this build can read, here a typed record of a later
// version, is replaced as corrupt by any nomination. An old reader does the
// same to every typed record, which is why the flag waits for every reader.
func TestNominateResidentReplacesAnUnreadableRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		clientId, instanceId := server.NewId(), server.NewId()
		later := encodeNetworkClientResidentTyped(residentCodecDbTestNomination(clientId, instanceId))
		later[1] = networkClientResidentVersion + 1
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Set(ctx, residentKey(clientId), later, time.Minute).Err(); err != nil {
				t.Fatal(err)
			}
		})
		if GetResidentForClient(ctx, clientId, 0) != nil {
			t.Fatal("a record of a later version was read")
		}
		nomination := residentCodecDbTestNomination(clientId, instanceId)
		if !NominateResidentWithFormat(ctx, nil, nomination, time.Minute, NetworkClientResidentFormatGob) {
			t.Fatal("an unreadable record was not replaced")
		}
		assertNetworkClientResidentFieldsEqual(t, nomination, GetResidentForClient(ctx, clientId, 0))
	})
}

// Removal of a record of either format: by another resident's id it is kept,
// by its own it is removed, with and without a deadline.
func TestRemoveResidentForClientAcrossFormats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, format := range residentCodecDbTestFormats() {
			for _, withDeadline := range []bool{false, true} {
				remove := func(clientId server.Id, residentId server.Id) {
					if withDeadline {
						if err := RemoveResidentForClientWithDeadline(ctx, clientId, residentId); err != nil {
							t.Fatal(err)
						}
					} else {
						RemoveResidentForClient(ctx, clientId, residentId)
					}
				}
				nomination := residentCodecDbTestNomination(server.NewId(), server.NewId())
				if !NominateResidentWithFormat(ctx, nil, nomination, time.Minute, format) {
					t.Fatal("not nominated")
				}
				record := Testing_ResidentRecord(ctx, nomination.ClientId)
				remove(nomination.ClientId, server.NewId())
				if !bytes.Equal(Testing_ResidentRecord(ctx, nomination.ClientId), record) {
					t.Fatalf("%s: removing another resident changed the record", format)
				}
				remove(nomination.ClientId, nomination.ResidentId)
				if 0 < len(Testing_ResidentRecord(ctx, nomination.ClientId)) {
					t.Fatalf("%s (deadline %t): the resident was not removed", format, withDeadline)
				}
			}
		}
	})
}

// Retirement captures a record of either format and removes it only while it
// is the same bytes: the same resident rewritten in the other format is a
// changed record, and survives.
func TestResidentRetirementAcrossFormats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, format := range residentCodecDbTestFormats() {
			nomination := residentCodecDbTestNomination(server.NewId(), server.NewId())
			if !NominateResidentWithFormat(ctx, nil, nomination, time.Minute, format) {
				t.Fatal("not nominated")
			}
			token, err := CaptureResidentForClientRetirement(ctx, nomination.ClientId, nomination.InstanceId)
			if err != nil || token == nil || token.residentId != nomination.ResidentId {
				t.Fatalf("%s: capture = %v", format, err)
			}
			if !bytes.Equal(token.value, Testing_ResidentRecord(ctx, nomination.ClientId)) {
				t.Fatalf("%s: the capture holds other bytes", format)
			}
			removed, err := RemoveCapturedResidentForClient(ctx, token)
			if err != nil || !removed || GetResidentForClient(ctx, nomination.ClientId, 0) != nil {
				t.Fatalf("%s: the captured record was not removed: %v", format, err)
			}

			// the same resident, rewritten in the other format after the capture
			otherFormat := NetworkClientResidentFormatTyped
			if format == NetworkClientResidentFormatTyped {
				otherFormat = NetworkClientResidentFormatGob
			}
			if !NominateResidentWithFormat(ctx, nil, nomination, time.Minute, format) {
				t.Fatal("not nominated again")
			}
			token, err = CaptureResidentForClientRetirement(ctx, nomination.ClientId, nomination.InstanceId)
			if err != nil || token == nil {
				t.Fatalf("%s: capture = %v", format, err)
			}
			rewritten := encodeNetworkClientResident(otherFormat, nomination)
			server.Redis(ctx, func(r server.RedisClient) {
				if err := r.Set(ctx, residentKey(nomination.ClientId), rewritten, time.Minute).Err(); err != nil {
					t.Fatal(err)
				}
			})
			removed, err = RemoveCapturedResidentForClient(ctx, token)
			if err != nil || removed {
				t.Fatalf("%s: a record rewritten as %s was removed: %v", format, otherFormat, err)
			}
			if !bytes.Equal(Testing_ResidentRecord(ctx, nomination.ClientId), rewritten) {
				t.Fatalf("%s: the rewritten record changed", format)
			}
		}
	})
}
