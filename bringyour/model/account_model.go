package model

import (
	// "context"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
	// "bringyour.com/bringyour/ulid"
)


type PreferencesSetArgs struct {
	ProductUpdates bool `json:"product_updates"`
}

type PreferencesSetResult struct {
}

func PreferencesSet(
	preferencesSet PreferencesSetArgs,
	session *session.ClientSession,
) (*PreferencesSetResult, error) {
	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		_, err := tx.Exec(
			session.Ctx,
			`
				INSERT INTO account_preferences (network_id, product_updates)
				VALUES ($1, $2)
				ON CONFLICT (network_id) DO UPDATE SET product_updates = $2
			`,
			session.ByJwt.NetworkId,
			preferencesSet.ProductUpdates,
		)
		bringyour.Raise(err)
	})

	result := &PreferencesSetResult{}
	return result, nil
}


type FeedbackSendArgs struct {
	Uses FeedbackSendUses `json:"uses"`
	Needs FeedbackSendNeeds `json:"needs"`
}
type FeedbackSendUses struct {
	Personal bool `json:"personal"`
	Business bool `json:"business"`
}
type FeedbackSendNeeds struct {
	Private bool `json:"private"`
	Safe bool `json:"safe"`
	Global bool `json:"global"`
	Collaborate bool `json:"collaborate"`
	AppControl bool `json:"app_control"`
	BlockDataBrokers bool `json:"block_data_brokers"`
	BlockAds bool `json:"block_ads"`
	Focus bool `json:"focus"`
	ConnectServers bool `json:"connect_servers"`
	RunServers bool `json:"run_servers"`
	PreventCyber bool `json:"prevent_cyber"`
	Audit bool `json:"audit"`
	ZeroTrust bool `json:"zero_trust"`
	Visualize bool `json:"visualize"`
	Other *string `json:"other"`
}

type FeedbackSendResult struct {
}


func FeedbackSend(
	feedbackSend FeedbackSendArgs,
	session *session.ClientSession,
) (*FeedbackSendResult, error) {
	bringyour.Tx(session.Ctx, func(tx bringyour.PgTx) {
		feedbackId := bringyour.NewId()
		_, err := tx.Exec(
			session.Ctx,
			`
				INSERT INTO account_feedback
				(
					feedback_id,
					network_id,
					user_id,
					uses_personal,
					uses_business,
					needs_private,
					needs_safe,
					needs_global,
					needs_collaborate,
					needs_app_control,
					needs_block_data_brokers,
					needs_block_ads,
					needs_focus,
					needs_connect_servers,
					needs_run_servers,
					needs_prevent_cyber,
					needs_audit,
					needs_zero_trust,
					needs_visualize,
					needs_other
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
			`,
			&feedbackId,
			&session.ByJwt.NetworkId,
			&session.ByJwt.UserId,
			feedbackSend.Uses.Personal,
			feedbackSend.Uses.Business,
			feedbackSend.Needs.Private,
			feedbackSend.Needs.Safe,
			feedbackSend.Needs.Global,
			feedbackSend.Needs.Collaborate,
			feedbackSend.Needs.AppControl,
			feedbackSend.Needs.BlockDataBrokers,
			feedbackSend.Needs.BlockAds,
			feedbackSend.Needs.Focus,
			feedbackSend.Needs.ConnectServers,
			feedbackSend.Needs.RunServers,
			feedbackSend.Needs.PreventCyber,
			feedbackSend.Needs.Audit,
			feedbackSend.Needs.ZeroTrust,
			feedbackSend.Needs.Visualize,
			feedbackSend.Needs.Other,
		)
		bringyour.Raise(err)
	})

	result := &FeedbackSendResult{}
	return result, nil
}

