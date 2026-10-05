package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Optional cleanup has exactly one generation authority: the abandoned
// nomination. A replacement, including another instance, must survive it.
func TestRemoveAbandonedResidentPreservesReplacement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		clientId, instanceId := server.NewId(), server.NewId()
		first := &model.NetworkClientResident{ClientId: clientId, InstanceId: instanceId, ResidentId: server.NewId()}
		if !model.NominateResident(ctx, nil, first, time.Minute) {
			t.Fatal("could not create first synthetic nomination")
		}
		if err := model.RemoveResidentForClientWithDeadline(ctx, clientId, first.ResidentId); err != nil {
			t.Fatal(err)
		}
		if current := model.GetResidentForClient(ctx, clientId, 0); current != nil {
			t.Fatal("abandoned generation was not removed")
		}
		if !model.NominateResident(ctx, nil, first, time.Minute) {
			t.Fatal("could not recreate first synthetic nomination")
		}
		replacement := &model.NetworkClientResident{ClientId: clientId, InstanceId: instanceId, ResidentId: server.NewId()}
		if !model.NominateResident(ctx, &first.ResidentId, replacement, time.Minute) {
			t.Fatal("could not replace synthetic nomination")
		}
		if err := model.RemoveResidentForClientWithDeadline(ctx, clientId, first.ResidentId); err != nil {
			t.Fatal(err)
		}
		if current := model.GetResidentForClient(ctx, clientId, 0); current == nil || current.ResidentId != replacement.ResidentId {
			t.Fatal("abandoned generation cleanup removed its replacement")
		}
		if err := model.RemoveResidentForClientWithDeadline(ctx, clientId, replacement.ResidentId); err != nil {
			t.Fatal(err)
		}
	})
}
