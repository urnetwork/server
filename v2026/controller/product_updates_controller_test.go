package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	// "github.com/urnetwork/server/v2026/jwt"
	// "github.com/urnetwork/server/v2026/model"
	// "github.com/urnetwork/server/v2026/session"
)

func TestBrevo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		n := 4
		userEmails := []string{}
		for range n {
			userEmail := fmt.Sprintf("test.%s@ur.io", server.NewId())
			userEmails = append(userEmails, userEmail)
		}

		for _, userEmail := range userEmails {
			err := BrevoAddContact(ctx, userEmail)
			connect.AssertEqual(t, err, nil)
		}

		// adding duplicate should be ok
		for _, userEmail := range userEmails {
			err := BrevoAddContact(ctx, userEmail)
			connect.AssertEqual(t, err, nil)
		}

		for _, userEmail := range userEmails {
			err := BrevoAddToList(ctx, userEmail, newNetworksListId())
			connect.AssertEqual(t, err, nil)
			err = BrevoAddToList(ctx, userEmail, productUpdatesListId())
			connect.AssertEqual(t, err, nil)
		}

		// adding duplicate should be ok
		for _, userEmail := range userEmails {
			err := BrevoAddToList(ctx, userEmail, newNetworksListId())
			connect.AssertEqual(t, err, nil)
			err = BrevoAddToList(ctx, userEmail, productUpdatesListId())
			connect.AssertEqual(t, err, nil)
		}

		for _, userEmail := range userEmails {
			err := BrevoRemoveFromList(ctx, userEmail, newNetworksListId())
			connect.AssertEqual(t, err, nil)
			err = BrevoRemoveFromList(ctx, userEmail, productUpdatesListId())
			connect.AssertEqual(t, err, nil)
		}

		// removing duplicate should be ok
		for _, userEmail := range userEmails {
			err := BrevoRemoveFromList(ctx, userEmail, newNetworksListId())
			connect.AssertEqual(t, err, nil)
			err = BrevoRemoveFromList(ctx, userEmail, productUpdatesListId())
			connect.AssertEqual(t, err, nil)
		}

		for _, userEmail := range userEmails {
			err := BrevoRemoveContact(ctx, userEmail)
			connect.AssertEqual(t, err, nil)
		}

		// removing duplicate should be ok
		for _, userEmail := range userEmails {
			err := BrevoRemoveContact(ctx, userEmail)
			connect.AssertEqual(t, err, nil)
		}

	})
}
