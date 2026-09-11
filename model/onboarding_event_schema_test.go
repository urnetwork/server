package model

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// TestValidateClientEvent pins the closed schema: exact names, exact prop keys,
// typed values, required props, and the server-only names refused from clients.
func TestValidateClientEvent(t *testing.T) {
	// happy paths
	props, err := ValidateClientEvent(EventOnboardingStepShown, map[string]any{
		"step": "plan", "index": float64(2), "elapsed_ms": float64(1500),
	})
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, "plan", props["step"])
	connect.AssertEqual(t, int64(2), props["index"])
	connect.AssertEqual(t, int64(1500), props["elapsed_ms"])

	props, err = ValidateClientEvent(EventOfferScreenShown, map[string]any{
		"surface": "intro_step", "experiment": "offer_screen", "variant": "control",
		"tier": "regional", "price_shown": 3.0, "currency": "USD", "expires_in_s": float64(432000),
	})
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, 3.0, props["price_shown"])

	_, err = ValidateClientEvent(EventOfferCtaTapped, map[string]any{"plan": "yearly", "store": "play"})
	connect.AssertEqual(t, nil, err)
	_, err = ValidateClientEvent(EventOfferDeclined, map[string]any{"control": "free_plan_link", "elapsed_ms": float64(10)})
	connect.AssertEqual(t, nil, err)
	_, err = ValidateClientEvent(EventPurchaseFailed, map[string]any{
		"store": "stripe", "product": "pro_yearly", "plan": "yearly", "trial": true,
		"price": 29.99, "currency": "USD", "error_class": "card_declined",
	})
	connect.AssertEqual(t, nil, err)
	_, err = ValidateClientEvent(EventConnectFirst, nil)
	connect.AssertEqual(t, nil, err)
	_, err = ValidateClientEvent(EventWidgetAdded, map[string]any{"kind": "globe"})
	connect.AssertEqual(t, nil, err)
	props, err = ValidateClientEvent(EventFeedbackSubmitted, map[string]any{
		"rating": float64(4), "reason": "too_slow", "has_text": true, "text": "  works   well ",
	})
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, "works   well", props["text"])
	_, err = ValidateClientEvent(EventSignupOptoutChanged, map[string]any{"product_updates": false})
	connect.AssertEqual(t, nil, err)

	// nil prop values are dropped, not rejected
	props, err = ValidateClientEvent(EventOnboardingStepSkipped, map[string]any{"step": "widgets", "index": nil})
	connect.AssertEqual(t, nil, err)
	_, hasIndex := props["index"]
	connect.AssertEqual(t, false, hasIndex)

	// refusals
	refused := func(name string, props map[string]any, fragment string) {
		_, err := ValidateClientEvent(name, props)
		if err == nil {
			t.Errorf("%s %v: expected a refusal", name, props)
			return
		}
		if _, ok := err.(*EventValidationError); !ok {
			t.Errorf("%s: expected an EventValidationError, got %T", name, err)
		}
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("%s: expected %q in %q", name, fragment, err.Error())
		}
	}
	refused("offer.shown", nil, "unknown event name")
	refused(EventLandingClicked, map[string]any{"step": "e1"}, "written by the server")
	refused(EventEmailSent, map[string]any{"step": "e1"}, "written by the server")
	refused(EventRetentionD7, nil, "written by the server")
	refused(EventOnboardingStepShown, map[string]any{"step": "plan", "user_email": "a@b.c"}, "unknown prop")
	refused(EventOnboardingStepShown, map[string]any{"index": float64(1)}, "missing prop \"step\"")
	refused(EventOnboardingStepShown, map[string]any{"step": "has space"}, "token")
	refused(EventOnboardingStepShown, map[string]any{"step": strings.Repeat("x", 65)}, "token")
	refused(EventOnboardingStepShown, map[string]any{"step": "plan", "index": 1.5}, "integer")
	refused(EventOnboardingStepShown, map[string]any{"step": "plan", "index": float64(-1)}, "in [0, 1000]")
	refused(EventOfferScreenShown, map[string]any{"surface": "popup"}, "one of")
	refused(EventOfferScreenShown, map[string]any{"surface": "intro_step", "currency": "DOLLARS"}, "token of at most 3")
	refused(EventOfferCtaTapped, map[string]any{"plan": "yearly", "store": "paypal"}, "one of")
	refused(EventPurchaseStarted, map[string]any{"store": "apple", "trial": "yes"}, "boolean")
	refused(EventPurchaseStarted, map[string]any{"store": "apple", "price": "free"}, "number")
	refused(EventFeedbackSubmitted, map[string]any{"rating": float64(6)}, "in [1, 5]")
	refused(EventFeedbackSubmitted, map[string]any{"text": strings.Repeat("a", 2001)}, "at most 2000")
	refused(EventFeedbackSubmitted, map[string]any{"text": "\xff\xfe"}, "utf-8")
	refused(EventSignupOptoutChanged, map[string]any{}, "missing prop")
	refused(EventConnectFirst, map[string]any{"anything": 1}, "unknown prop")
}

func TestEventSchemaLists(t *testing.T) {
	clientNames := ClientEventNames()
	connect.AssertEqual(t, 15, len(clientNames))
	for _, name := range clientNames {
		spec := EventSpecFor(name)
		connect.AssertEqual(t, false, spec.ServerOnly)
		connect.AssertEqual(t, EventOwnerClient, spec.Owner)
	}
	serverNames := ServerEventNames()
	connect.AssertEqual(t, 15, len(serverNames))
	for _, name := range serverNames {
		spec := EventSpecFor(name)
		connect.AssertEqual(t, true, spec.ServerOnly)
		connect.AssertEqual(t, true, spec.Owner != EventOwnerClient)
	}
	connect.AssertEqual(t, true, EventSpecFor("nope") == nil)
	// connect.day is the server's connection history: never from a client
	connect.AssertEqual(t, true, EventSpecFor(EventConnectDay).ServerOnly)
	_, err := ValidateClientEvent(EventConnectDay, map[string]any{})
	connect.AssertEqual(t, true, err != nil)
	connect.AssertEqual(t, []string{"text"}, EventTextPropKeys(EventFeedbackSubmitted))
	connect.AssertEqual(t, 0, len(EventTextPropKeys(EventConnectFirst)))

	// the server may write any name, client names included
	_, err = ValidateServerEvent(EventSignupOptoutChanged, map[string]any{"product_updates": true})
	connect.AssertEqual(t, nil, err)
	_, err = ValidateServerEvent(EventAppOpened, map[string]any{"step": "e1_connect", "flow_step": "e1"})
	connect.AssertEqual(t, nil, err)
	_, err = ValidateServerEvent(EventEmailOpened, map[string]any{"step": "e3_last_chance", "flow_step": "e5"})
	connect.AssertEqual(t, nil, err)
	_, err = ValidateServerEvent(EventEmailOpened, map[string]any{"step": "e3_last_chance", "flow_step": "e6"})
	connect.AssertEqual(t, true, err != nil)
	_, err = ValidateServerEvent(EventAppOpened, map[string]any{"token": "x"})
	connect.AssertEqual(t, true, err != nil)
}

func TestIsEventToken(t *testing.T) {
	connect.AssertEqual(t, true, IsEventToken("pro_yearly.v2:a+b-c", 64))
	connect.AssertEqual(t, false, IsEventToken("", 64))
	connect.AssertEqual(t, false, IsEventToken("a b", 64))
	connect.AssertEqual(t, false, IsEventToken("é", 64))
	connect.AssertEqual(t, false, IsEventToken("abc", 2))
}
