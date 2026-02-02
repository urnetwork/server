package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2026"
	// "github.com/urnetwork/server/v2026/jwt"
	// "github.com/urnetwork/server/v2026/model"
	// "github.com/urnetwork/server/v2026/session"
)

func TestBrevo(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

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
			assert.Equal(t, err, nil)
		}

		// adding duplicate should be ok
		for _, userEmail := range userEmails {
			err := BrevoAddContact(ctx, userEmail)
			assert.Equal(t, err, nil)
		}

		for _, userEmail := range userEmails {
			err := BrevoAddToList(ctx, userEmail, newNetworksListId())
			assert.Equal(t, err, nil)
			err = BrevoAddToList(ctx, userEmail, productUpdatesListId())
			assert.Equal(t, err, nil)
		}

		// adding duplicate should be ok
		for _, userEmail := range userEmails {
			err := BrevoAddToList(ctx, userEmail, newNetworksListId())
			assert.Equal(t, err, nil)
			err = BrevoAddToList(ctx, userEmail, productUpdatesListId())
			assert.Equal(t, err, nil)
		}

		for _, userEmail := range userEmails {
			err := BrevoRemoveFromList(ctx, userEmail, newNetworksListId())
			assert.Equal(t, err, nil)
			err = BrevoRemoveFromList(ctx, userEmail, productUpdatesListId())
			assert.Equal(t, err, nil)
		}

		// removing duplicate should be ok
		for _, userEmail := range userEmails {
			err := BrevoRemoveFromList(ctx, userEmail, newNetworksListId())
			assert.Equal(t, err, nil)
			err = BrevoRemoveFromList(ctx, userEmail, productUpdatesListId())
			assert.Equal(t, err, nil)
		}

		for _, userEmail := range userEmails {
			err := BrevoRemoveContact(ctx, userEmail)
			assert.Equal(t, err, nil)
		}

		// removing duplicate should be ok
		for _, userEmail := range userEmails {
			err := BrevoRemoveContact(ctx, userEmail)
			assert.Equal(t, err, nil)
		}

	})
}
