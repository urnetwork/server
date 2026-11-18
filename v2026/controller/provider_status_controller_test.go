package controller

// The order of the provider statuses an answer returns.

import (
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The caller's own provider comes first, once; the rest keep their order.
func TestOrderProviderStatuses(t *testing.T) {
	a := &model.ProviderStatus{ClientId: server.NewId()}
	b := &model.ProviderStatus{ClientId: server.NewId()}
	c := &model.ProviderStatus{ClientId: server.NewId()}
	ids := func(statuses []*model.ProviderStatus) []server.Id {
		clientIds := []server.Id{}
		for _, status := range statuses {
			clientIds = append(clientIds, status.ClientId)
		}
		return clientIds
	}
	check := func(got []*model.ProviderStatus, want ...*model.ProviderStatus) {
		t.Helper()
		gotIds := ids(got)
		wantIds := ids(want)
		if len(gotIds) != len(wantIds) {
			t.Fatalf("got %v, want %v", gotIds, wantIds)
		}
		for i := range gotIds {
			if gotIds[i] != wantIds[i] {
				t.Fatalf("got %v, want %v", gotIds, wantIds)
			}
		}
	}

	check(orderProviderStatuses([]*model.ProviderStatus{a, b, c}, &c.ClientId), c, a, b)
	check(orderProviderStatuses([]*model.ProviderStatus{a, b, c}, nil), a, b, c)
	otherClientId := server.NewId()
	check(orderProviderStatuses([]*model.ProviderStatus{a, b, c}, &otherClientId), a, b, c)
	// the caller fetched separately and also in the list appears once
	check(orderProviderStatuses([]*model.ProviderStatus{b, a, b, c}, &b.ClientId), b, a, c)
	if !providerStatusesContain([]*model.ProviderStatus{a, b}, b.ClientId) || providerStatusesContain([]*model.ProviderStatus{a, b}, c.ClientId) {
		t.Fatal("contain")
	}
}
