package connect

import (
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server/internal/privateheapprofile"
)

// privateHeapProfileCompanion reads fixed accounting state without walking
// residents, connections, queues, frames, or payload bytes. Pool counters are
// coherent within each size class, not one atomic instant across all classes;
// take-minus-return reconciles ownership and does not prove reachable heap.
func privateHeapProfileCompanion(exchange *Exchange, handler *ConnectHandler) privateheapprofile.Companion {
	var out privateheapprofile.Companion
	out.Available = exchange != nil && handler != nil && handler.exchange == exchange &&
		exchange.ctx != nil && handler.ctx != nil
	if handler != nil {
		// This visits only the configured listener-state entries, not accepted
		// sockets. Keep listener readiness separate from lifecycle admission.
		out.ListenerReady = handler.ListenerReady() == nil
		if out.Available {
			handler.activeLock.Lock()
			out.ServingActive = !handler.closing && handler.ctx.Err() == nil &&
				exchange.ctx.Err() == nil && !exchange.IsDraining()
			handler.activeLock.Unlock()
		}
	}

	classes := clientconnect.GetMessagePoolClassStats()
	expectedSizes := [...]int{256, 2048, 4096, 8192}
	if len(classes) == len(expectedSizes) {
		complete := true
		for i, size := range expectedSizes {
			if classes[i] == nil || classes[i].Size != size {
				complete = false
				break
			}
		}
		if complete {
			out.PoolClassesComplete = true
			for i, class := range classes {
				out.PoolClasses[i] = privateheapprofile.PoolClass{
					Size: class.Size, Capacity: class.Capacity, Retained: class.Retained,
					Taken: class.Taken, Returned: class.Returned, Created: class.Created,
				}
			}
		}
	}

	if exchange != nil {
		snapshot := exchange.residentPayloadLedger().snapshot()
		out.ResidentPayload = &privateheapprofile.ResidentPayloadSnapshot{
			Enabled: snapshot.Enabled, Complete: snapshot.Complete,
		}
		// An incomplete sample contains no authoritative ownership values.
		// Consumers must check Complete before interpreting the fixed fields.
		if snapshot.Enabled && snapshot.Complete {
			names := [...]string{"control_ingress", "forward_ingress", "forward_output"}
			for i, group := range snapshot.Groups {
				out.ResidentPayload.Stages[i] = privateheapprofile.ResidentPayloadStage{
					Stage: names[i], Messages: group.Messages, LogicalBytes: group.LogicalBytes,
					BackingByteCharges: group.BackingCharge,
					AdmittedTotal:      group.Admitted, ReleasedTotal: group.Released,
				}
			}
		}
	}
	if exchange != nil && exchange.sdkPayloadOwnerLedger != nil {
		out.SDKPayload = privateHeapSDKPayloadSnapshot(exchange.sdkPayloadOwnerLedger.Snapshot())
	}
	// A nil SDKPayload means this diagnostic scope is disabled, not an
	// observed zero-byte ACK or forward tail.
	return out
}

func privateHeapSDKPayloadSnapshot(snapshot clientconnect.TransferPayloadOwnerSnapshot) *privateheapprofile.TransferPayloadOwnerSnapshot {
	out := &privateheapprofile.TransferPayloadOwnerSnapshot{
		Enabled: snapshot.Enabled, Complete: snapshot.Complete, Revision: snapshot.Revision,
	}
	if snapshot.Enabled && snapshot.Complete {
		// These are lifetime cap charges. Shared roots can appear repeatedly;
		// neither group is an additive physical-heap or queue-occupancy measure.
		out.SendAck = privateheapprofile.TransferPayloadOwnerGroup{
			Owners: snapshot.SendAck.Owners, BackingByteCharges: snapshot.SendAck.BackingByteCharges,
			AdmittedTotal: snapshot.SendAck.AdmittedTotal, ReleasedTotal: snapshot.SendAck.ReleasedTotal,
		}
		out.Forward = privateheapprofile.TransferPayloadOwnerGroup{
			Owners: snapshot.Forward.Owners, BackingByteCharges: snapshot.Forward.BackingByteCharges,
			AdmittedTotal: snapshot.Forward.AdmittedTotal, ReleasedTotal: snapshot.Forward.ReleasedTotal,
		}
	}
	return out
}
