package model

import (
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type FeedbackSendArgs struct {
	Uses      FeedbackSendUses  `json:"uses"`
	Needs     FeedbackSendNeeds `json:"needs"`
	StarCount int               `json:"star_count"`
}
type FeedbackSendUses struct {
	Personal bool `json:"personal"`
	Business bool `json:"business"`
}
type FeedbackSendNeeds struct {
	Private          bool    `json:"private"`
	Safe             bool    `json:"safe"`
	Global           bool    `json:"global"`
	Collaborate      bool    `json:"collaborate"`
	AppControl       bool    `json:"app_control"`
	BlockDataBrokers bool    `json:"block_data_brokers"`
	BlockAds         bool    `json:"block_ads"`
	Focus            bool    `json:"focus"`
	ConnectServers   bool    `json:"connect_servers"`
	RunServers       bool    `json:"run_servers"`
	PreventCyber     bool    `json:"prevent_cyber"`
	Audit            bool    `json:"audit"`
	ZeroTrust        bool    `json:"zero_trust"`
	Visualize        bool    `json:"visualize"`
	Other            *string `json:"other"`
}

type FeedbackSendResult struct {
	FeedbackId server.Id `json:"feedback_id"`
}

func FeedbackSend(
	feedbackSend FeedbackSendArgs,
	session *session.ClientSession,
) (*FeedbackSendResult, error) {

	var feedbackId server.Id

	server.Tx(session.Ctx, func(tx server.PgTx) {
		feedbackId = server.NewId()
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
					needs_other,
					star_count
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
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
			feedbackSend.StarCount,
		)
		server.Raise(err)
	})

	result := &FeedbackSendResult{
		FeedbackId: feedbackId,
	}
	return result, nil
}

type GetFeedbackResult struct {
	FeedbackId server.Id
	NetworkId  server.Id
}

func GetFeedbackById(
	feedbackId *server.Id,
	session *session.ClientSession,
) (feedback *GetFeedbackResult, err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			session.Ctx,
			`
					SELECT
						feedback_id,
						network_id
					FROM
						account_feedback
					WHERE feedback_id = $1
				`,
			feedbackId,
		)

		server.WithPgResult(result, err, func() {

			if result.Next() {

				feedback = &GetFeedbackResult{}

				server.Raise(
					result.Scan(
						&feedback.FeedbackId,
						&feedback.NetworkId,
					),
				)
			}
		})
	})
	return
}
