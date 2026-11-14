package controller

// Delivery of a paid gift order (GIFTOFPRIVACY.md §2.5, §10.4, §10.6 part 3):
// the cards as PDFs, the emails that carry them, and the retention that removes
// them.
//
// The payment's transaction schedules the render task (ScheduleGiftDeliveryInTx,
// one per order). The task prints the PDFs (gift_cards_pdf.go) and stores them,
// every card last so that the order reads as PdfReady only once each friend's
// card is stored too. Its post, in the task's finishing transaction, owes the
// emails through the account message outbox: the buyer's now, with every card;
// for delivery "them", each friend's at 9:00 on the buyer's chosen date in the
// buyer's zone (or now), with their own card, from URnetwork with the buyer as
// reply-to. The outbox keeps only the order's id (and the friend's index): an
// email reads its order, codes and card when it is sent, so a PDF never sits in
// the outbox and an order refunded before its date sends nothing more.
//
// Every day the retention removes the PDFs, the note, the signature and the
// friends' addresses (model.RemoveGiftOrderPersonalData) of each order whose
// last email went out 30 days ago or more (one refunded before any email was
// owed, 30 days after its refund), of each order still unpaid 30 days after it
// was made, and of each refunded before it was paid, 30 days after its refund.
// The codes and the buyer's address stay, so a late coin payment can still be
// fulfilled and handled by hand: its codes are made, its buyer gets every card,
// and no friend is emailed. An unpaid card order also loses its buyer's address
// (model.RemoveGiftOrderBuyerEmail), as a Stripe session cannot be paid late,
// and so does an order of either rail refunded before it was paid: nothing
// needs it after that.

import (
	"context"
	"fmt"
	htmltemplate "html/template"
	"slices"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The bound on one render: up to 21 PDFs in one browser.
const giftRenderCardsMaxTime = 5 * time.Minute

// The email templates.
const (
	giftCardsBuyerTemplateName = "gift_cards_buyer"
	giftCardFriendTemplateName = "gift_card_friend"
)

// The attachments' names.
const (
	giftCardsBuyerPdfFileName = "URnetwork-gift-cards.pdf"
	giftCardFriendPdfFileName = "URnetwork-gift-card.pdf"
)

// The outbox key of the buyer's email, and of each friend's.
func giftBuyerEmailKey(giftOrderId server.Id) string {
	return giftOrderId.String()
}

func giftFriendEmailKey(giftOrderId server.Id, recipientIndex int) string {
	return fmt.Sprintf("%s/%s", giftOrderId, model.GiftOrderPdfRecipientPart(recipientIndex))
}

// The render task's chain key: one render of an order at a time, and a second
// schedule while one is pending merges into it.
func giftRenderCardsRunOnce(giftOrderId server.Id) *task.RunOnceOption {
	return task.RunOnce("gift_render_cards", giftOrderId)
}

// Owes the delivery of a newly paid order, in the transaction that pays it: the
// render task, which prints the cards and then owes the emails. Called once per
// order (FulfillGiftOrderInTx's newlyPaid); a second call merges into the
// pending render.
func ScheduleGiftDeliveryInTx(tx server.PgTx, ctx context.Context, giftOrderId server.Id) (returnErr error) {
	server.HandleError(func() {
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		task.ScheduleTaskInTx(
			tx,
			GiftRenderCards,
			&GiftRenderCardsArgs{
				GiftOrderId: giftOrderId,
			},
			clientSession,
			giftRenderCardsRunOnce(giftOrderId),
			task.MaxTime(giftRenderCardsMaxTime),
		)
	}, func(err error) {
		returnErr = fmt.Errorf("could not schedule the delivery of gift order %s: %w", giftOrderId, err)
	})
	return
}

type GiftRenderCardsArgs struct {
	GiftOrderId server.Id `json:"gift_order_id"`
}

type GiftRenderCardsResult struct {
	// false when the order is not paid (refunded first, or unknown): nothing is
	// printed and nothing owed
	Rendered bool `json:"rendered"`
	// the parts stored, "all" last
	Parts []string `json:"parts,omitempty"`
}

// Prints a paid order's cards and stores them: each friend's card, then every
// card ("all"), which makes the order PdfReady. A failure is retried by the
// task; stored parts are replaced on the next attempt.
func GiftRenderCards(
	args *GiftRenderCardsArgs,
	clientSession *session.ClientSession,
) (*GiftRenderCardsResult, error) {
	ctx := clientSession.Ctx
	order := model.GetGiftOrder(ctx, args.GiftOrderId)
	if order == nil || order.Status != model.GiftOrderStatusPaid {
		return &GiftRenderCardsResult{Rendered: false}, nil
	}
	codes := model.GetGiftOrderCodes(ctx, order.GiftOrderId)
	if len(codes) != order.Quantity {
		return nil, fmt.Errorf("gift order %s has %d codes for %d gifts", order.GiftOrderId, len(codes), order.Quantity)
	}
	pdfs, err := renderGiftOrderPdfs(ctx, order, codes)
	if err != nil {
		return nil, err
	}
	parts := []string{}
	for part := range pdfs {
		if part != model.GiftOrderPdfPartAll {
			parts = append(parts, part)
		}
	}
	slices.Sort(parts)
	parts = append(parts, model.GiftOrderPdfPartAll)
	for _, part := range parts {
		if err := model.PutGiftOrderPdf(ctx, order.GiftOrderId, part, pdfs[part]); err != nil {
			return nil, err
		}
	}
	glog.Infof("[gift]printed the cards of gift order %s: %d parts\n", order.GiftOrderId, len(parts))
	return &GiftRenderCardsResult{
		Rendered: true,
		Parts:    parts,
	}, nil
}

// Owes the order's emails, in the task's finishing transaction.
func GiftRenderCardsPost(
	args *GiftRenderCardsArgs,
	result *GiftRenderCardsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if !result.Rendered {
		return nil
	}
	order := model.GetGiftOrder(clientSession.Ctx, args.GiftOrderId)
	if order == nil || order.Status != model.GiftOrderStatusPaid {
		return nil
	}
	addGiftOrderEmailsInTx(clientSession.Ctx, tx, order, server.NowUtc())
	return nil
}

// Owes the buyer's email now and, for delivery "them", each friend's at its
// time. Each key is the order's, so owing them again adds nothing.
func addGiftOrderEmailsInTx(ctx context.Context, tx server.PgTx, order *model.GiftOrder, now time.Time) {
	addAccountMessageInTx(ctx, tx, &accountMessage{
		key:      giftBuyerEmailKey(order.GiftOrderId),
		userAuth: order.BuyerEmail,
		template: &GiftCardsBuyerTemplate{
			GiftOrderId: order.GiftOrderId,
		},
	})
	sendAt := GiftFriendsSendAt(order)
	if sendAt == nil {
		return
	}
	// a time already past is due as it is owed, not before
	deliverTime := *sendAt
	if deliverTime.Before(now) {
		deliverTime = now.UTC()
	}
	for recipientIndex, recipient := range order.Recipients {
		if strings.TrimSpace(recipient) == "" {
			continue
		}
		addAccountMessageInTx(ctx, tx, &accountMessage{
			key:      giftFriendEmailKey(order.GiftOrderId, recipientIndex),
			userAuth: recipient,
			template: &GiftCardFriendTemplate{
				GiftOrderId:    order.GiftOrderId,
				RecipientIndex: recipientIndex,
			},
			deliverTime: &deliverTime,
		})
	}
}

// The card hold (GIFTOFPRIVACY.md §9.2): the friends' emails of an order paid
// by card wait this long after the payment, past the early fraud warnings. A
// refund or a dispute in the hold voids the codes or ends the order, and each
// email is withdrawn when it comes due (GiftCardFriendTemplate's loader). A
// coin payment has no chargeback, so its emails have no hold.
const GiftFriendsCardHold = 2 * time.Hour

// GiftFriendsSendAt is when a paid order's friends' emails go out: 9:00 on its
// send-on date in the buyer's zone, or the payment for an order without a
// date or whose date had passed by then; never before the payment, and for an
// order not paid in a coin (by card) never before GiftFriendsCardHold after
// it. nil when the order emails no friend: delivery "me", no friend's address
// left, or not paid (pending, or refunded). The delivery owes the emails at
// this time, POST /pay/gift/order answers it (friends_send_at), and the
// buyer's email says it.
func GiftFriendsSendAt(order *model.GiftOrder) *time.Time {
	if order == nil || order.Status != model.GiftOrderStatusPaid || order.PaidTime == nil {
		return nil
	}
	if order.Delivery != model.GiftDeliveryThem || !giftOrderHasRecipients(order) {
		return nil
	}
	sendAt := giftFriendDeliverTime(order, *order.PaidTime)
	return &sendAt
}

// When the friends' emails of the order, paid at paidTime, are due, by
// GiftFriendsSendAt's rule whatever the order's state: the later of its
// scheduled time (9:00 on its send-on date in the buyer's zone, at the zone's
// offset that day) and the payment, plus the card hold unless it was paid in a
// coin.
func giftFriendDeliverTime(order *model.GiftOrder, paidTime time.Time) time.Time {
	earliest := paidTime.UTC()
	if order.Rail != model.GiftRailCrypto {
		earliest = earliest.Add(GiftFriendsCardHold)
	}
	if order.SendOn != nil {
		if scheduled := model.GiftOrderDeliverTime(*order.SendOn, order.SendTimezone); earliest.Before(scheduled) {
			return scheduled
		}
	}
	return earliest
}

// One code as an email lists it.
type giftEmailCode struct {
	// the code in groups, as the card prints it
	Code       string
	RedeemLink string
	RedeemBy   string
	// delivery "them": "For <friend>"
	For string
}

// What both gift emails read from their order when they are sent.
type giftEmailOrder struct {
	order *model.GiftOrder
	codes []*model.GiftCode
	pdf   []byte
	// when it was read to be sent; zero for one built in place, which reads
	// as sent at the payment
	loadTime time.Time
}

// When the email goes out: when it was read, else at the payment.
func (self *giftEmailOrder) sendTime() time.Time {
	if !self.loadTime.IsZero() {
		return self.loadTime
	}
	if self.order != nil && self.order.PaidTime != nil {
		return *self.order.PaidTime
	}
	return time.Time{}
}

func (self *giftEmailOrder) language() string {
	if self.order == nil {
		return giftCardLanguages[0]
	}
	return giftCardLanguage(self.order.Language)
}

// "Redeem by {date}": the last whole day the code works everywhere, as the
// cards print it (model.GiftRedeemByDate).
func (self *giftEmailOrder) redeemBy(code *model.GiftCode) string {
	language := self.language()
	return giftCardText(language, "card.redeem_by", "date", giftCardDate(language, model.GiftRedeemByDate(code.RedeemBy)))
}

// The functions every gift email's files call: `T` (giftCardText in the
// order's language) and `Upper` (the text part's headline).
func (self *giftEmailOrder) funcs(funcs texttemplate.FuncMap) {
	language := self.language()
	funcs["T"] = func(key string, args ...string) string {
		return giftCardText(language, key, args...)
	}
	funcs["Upper"] = strings.ToUpper
}

// Reads the order, its codes and one PDF part. A refunded or voided order
// withdraws the email; a missing PDF is retried (the render may still run).
func (self *giftEmailOrder) load(ctx context.Context, giftOrderId server.Id, part string) error {
	order := model.GetGiftOrder(ctx, giftOrderId)
	if order == nil {
		return fmt.Errorf("gift order %s does not exist: %w", giftOrderId, errAccountMessageWithdrawn)
	}
	switch order.Status {
	case model.GiftOrderStatusPaid:
	case model.GiftOrderStatusRefunded:
		return fmt.Errorf("gift order %s was refunded: %w", giftOrderId, errAccountMessageWithdrawn)
	default:
		return fmt.Errorf("gift order %s is %s", giftOrderId, order.Status)
	}
	codes := model.GetGiftOrderCodes(ctx, giftOrderId)
	if len(codes) == 0 {
		return fmt.Errorf("gift order %s has no codes", giftOrderId)
	}
	pdf := model.GetGiftOrderPdf(ctx, giftOrderId, part)
	if len(pdf) == 0 {
		return fmt.Errorf("gift order %s has no %s card yet", giftOrderId, part)
	}
	self.order = order
	self.codes = giftOrderCards(order, codes)
	self.pdf = pdf
	self.loadTime = server.NowUtc()
	return nil
}

// The buyer's email (gift_cards_buyer): every card of the order in one PDF, and
// every code. For delivery "them", it also says when the friends get theirs.
type GiftCardsBuyerTemplate struct {
	GiftOrderId server.Id `json:"gift_order_id"`

	// read when sent (LoadAccountMessage), never stored
	giftEmailOrder
	BaseTemplate
}

func (self *GiftCardsBuyerTemplate) Name() string {
	return giftCardsBuyerTemplateName
}

func (self *GiftCardsBuyerTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	self.giftEmailOrder.funcs(funcs)
}

// The buyer's site language (LanguageTemplate).
func (self *GiftCardsBuyerTemplate) Language() string {
	return self.language()
}

// The order's codes, the combined PDF and the order itself
// (accountMessageLoader).
func (self *GiftCardsBuyerTemplate) LoadAccountMessage(ctx context.Context) error {
	return self.giftEmailOrder.load(ctx, self.GiftOrderId, model.GiftOrderPdfPartAll)
}

// Every card, one PDF (AttachmentTemplate).
func (self *GiftCardsBuyerTemplate) EmailAttachments() []*EmailAttachment {
	if len(self.pdf) == 0 {
		return nil
	}
	return []*EmailAttachment{{
		Filename:    giftCardsBuyerPdfFileName,
		ContentType: "application/pdf",
		Data:        self.pdf,
	}}
}

func (self *GiftCardsBuyerTemplate) count() int {
	return len(self.codes)
}

func (self *GiftCardsBuyerTemplate) Subject() string {
	return giftCardTextCount(self.language(), "buyer.subject", self.count())
}

func (self *GiftCardsBuyerTemplate) Preheader() string {
	return giftCardTextCount(self.language(), "buyer.preheader", self.count())
}

func (self *GiftCardsBuyerTemplate) Headline() string {
	return giftCardTextCount(self.language(), "buyer.headline", self.count())
}

// Delivery "them" with a friend to email: each friend gets their own card. An
// order whose addresses the retention removed (paid late) emails no friend, so
// its buyer's email reads as delivery "me".
func (self *GiftCardsBuyerTemplate) Them() bool {
	return self.order != nil && self.order.Delivery == model.GiftDeliveryThem && giftOrderHasRecipients(self.order)
}

// Whether the order has a friend's address to email.
func giftOrderHasRecipients(order *model.GiftOrder) bool {
	for _, recipient := range order.Recipients {
		if strings.TrimSpace(recipient) != "" {
			return true
		}
	}
	return false
}

// What the email is about: the cards attached, or when the friends get theirs
// (GiftFriendsSendAt): now, once that time has come when this email goes out
// (a coin payment's, or a late email); on the buyer's date; or, held after a
// card payment, at the time, in the buyer's zone (else UTC).
func (self *GiftCardsBuyerTemplate) Lead() string {
	language := self.language()
	if !self.Them() {
		return giftCardTextCount(language, "buyer.me", self.count())
	}
	sendAt := GiftFriendsSendAt(self.order)
	if sendAt == nil || !self.sendTime().Before(*sendAt) {
		return giftCardTextCount(language, "buyer.them_now", self.count())
	}
	location := model.GiftOrderLocation(self.order.SendTimezone)
	if self.order.SendOn != nil && sendAt.Equal(model.GiftOrderDeliverTime(*self.order.SendOn, self.order.SendTimezone)) {
		return giftCardTextCount(language, "buyer.them_on", self.count(), "date", giftCardDate(language, sendAt.In(location)))
	}
	return giftCardTextCount(language, "buyer.them_hold", self.count(), "time", giftCardTime(language, sendAt.In(location)))
}

// Delivery "them": the cards are here too.
func (self *GiftCardsBuyerTemplate) ThemCopy() string {
	return giftCardTextCount(self.language(), "buyer.them_copy", self.count())
}

func (self *GiftCardsBuyerTemplate) CodesTitle() string {
	return giftCardTextCount(self.language(), "buyer.codes", self.count())
}

func (self *GiftCardsBuyerTemplate) Codes() []*giftEmailCode {
	language := self.language()
	codes := []*giftEmailCode{}
	for _, code := range self.codes {
		emailCode := &giftEmailCode{
			Code:       giftCardCodeGrouped(code.Secret),
			RedeemLink: giftCardRedeemLink(code.Secret),
			RedeemBy:   self.redeemBy(code),
		}
		if self.Them() && code.RecipientIndex != nil && *code.RecipientIndex < len(self.order.Recipients) {
			if recipient := strings.TrimSpace(self.order.Recipients[*code.RecipientIndex]); recipient != "" {
				emailCode.For = giftCardText(language, "buyer.code_for", "email", recipient)
			}
		}
		codes = append(codes, emailCode)
	}
	return codes
}

// A friend's email (gift_card_friend): their own card and the buyer's note,
// from URnetwork with the buyer's address as reply-to, sent on the buyer's date.
type GiftCardFriendTemplate struct {
	GiftOrderId    server.Id `json:"gift_order_id"`
	RecipientIndex int       `json:"recipient_index"`

	// read when sent (LoadAccountMessage), never stored
	giftEmailOrder
	code *model.GiftCode
	BaseTemplate
}

func (self *GiftCardFriendTemplate) Name() string {
	return giftCardFriendTemplateName
}

func (self *GiftCardFriendTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	self.giftEmailOrder.funcs(funcs)
}

// The buyer's site language (LanguageTemplate).
func (self *GiftCardFriendTemplate) Language() string {
	return self.language()
}

// The order, the friend's code and card (accountMessageLoader). A voided code
// withdraws the email.
func (self *GiftCardFriendTemplate) LoadAccountMessage(ctx context.Context) error {
	if err := self.giftEmailOrder.load(ctx, self.GiftOrderId, model.GiftOrderPdfRecipientPart(self.RecipientIndex)); err != nil {
		return err
	}
	for _, code := range self.codes {
		if code.RecipientIndex != nil && *code.RecipientIndex == self.RecipientIndex {
			if code.Voided {
				return fmt.Errorf("gift order %s card %d was voided: %w", self.GiftOrderId, self.RecipientIndex, errAccountMessageWithdrawn)
			}
			if now := server.NowUtc(); !now.Before(code.RedeemBy) {
				// a scheduled card's window starts at its delivery, so this
				// should not happen; never send a friend a dead code
				giftReportExpiredFriendCard(ctx, self.GiftOrderId, self.RecipientIndex, code, now)
				return fmt.Errorf("gift order %s card %d expired before its email: %w", self.GiftOrderId, self.RecipientIndex, errAccountMessageWithdrawn)
			}
			self.code = code
			return nil
		}
	}
	return fmt.Errorf("gift order %s has no code for recipient %d", self.GiftOrderId, self.RecipientIndex)
}

// Once the friend's email is sent or given up, an order whose crypto invoice
// the 30-day unlink has passed forgets its buyer's address, kept as the friends'
// reply-to, when no friend's email still needs it
// (accountMessageFinisher; model.ClearUnlinkedGiftOrderBuyerEmail does
// nothing for any other order). The daily retention covers a missed one.
func (self *GiftCardFriendTemplate) AccountMessageFinished(ctx context.Context) {
	model.ClearUnlinkedGiftOrderBuyerEmail(ctx, self.GiftOrderId)
}

// Tells the operator a friend's card expired before its email went out: an
// error log and a payment reconciliation event (store "gift", action "error"),
// the trail operators read for payments that need a hand.
func giftReportExpiredFriendCard(ctx context.Context, giftOrderId server.Id, recipientIndex int, code *model.GiftCode, now time.Time) {
	glog.Errorf(
		"[gift]gift order %s card %d expired at %s before its friend's email; withdrawn\n",
		giftOrderId,
		recipientIndex,
		code.RedeemBy.Format(time.RFC3339),
	)
	err := model.AddPaymentReconciliationEvent(ctx, &model.PaymentReconciliationEvent{
		RunId:    server.NewId(),
		Store:    model.SubscriptionMarketGift,
		Action:   model.PaymentReconcileActionError,
		Evidence: giftOrderId.String(),
		Details: map[string]any{
			"reason":          "gift card expired before its friend's email",
			"gift_order_id":   giftOrderId.String(),
			"recipient_index": recipientIndex,
			"balance_code_id": code.BalanceCodeId.String(),
			"redeem_by":       code.RedeemBy.Format(time.RFC3339),
			"now":             now.Format(time.RFC3339),
		},
	})
	if err != nil {
		glog.Errorf("[gift]could not record the expired card of gift order %s: %s\n", giftOrderId, err)
	}
}

// The buyer (ReplyToTemplate).
func (self *GiftCardFriendTemplate) ReplyTo() string {
	if self.order == nil {
		return ""
	}
	return self.order.BuyerEmail
}

// The friend's card (AttachmentTemplate).
func (self *GiftCardFriendTemplate) EmailAttachments() []*EmailAttachment {
	if len(self.pdf) == 0 {
		return nil
	}
	return []*EmailAttachment{{
		Filename:    giftCardFriendPdfFileName,
		ContentType: "application/pdf",
		Data:        self.pdf,
	}}
}

// The buyer's signature on one line, or "".
func (self *GiftCardFriendTemplate) Signature() string {
	if self.order == nil {
		return ""
	}
	return strings.Join(strings.Fields(self.order.Signature), " ")
}

// "{signature} sent you a gift of privacy", or without a signature "Someone who
// cares about you sent you a gift of privacy".
func (self *GiftCardFriendTemplate) Subject() string {
	if signature := self.Signature(); signature != "" {
		return giftCardText(self.language(), "friend.subject.signed", "signature", signature)
	}
	return giftCardText(self.language(), "friend.subject.anonymous")
}

func (self *GiftCardFriendTemplate) Intro() string {
	if signature := self.Signature(); signature != "" {
		return giftCardText(self.language(), "friend.intro.signed", "signature", signature)
	}
	return giftCardText(self.language(), "friend.intro.anonymous")
}

// The buyer's note as the card prints it, or "".
func (self *GiftCardFriendTemplate) Note() string {
	if self.order == nil {
		return ""
	}
	return giftCardNote(self.order.Message)
}

// The note for the html part: escaped, its line breaks as <br>.
func (self *GiftCardFriendTemplate) NoteHtml() htmltemplate.HTML {
	lines := strings.Split(self.Note(), "\n")
	for i, line := range lines {
		lines[i] = htmltemplate.HTMLEscapeString(line)
	}
	return htmltemplate.HTML(strings.Join(lines, "<br>"))
}

func (self *GiftCardFriendTemplate) Code() string {
	if self.code == nil {
		return ""
	}
	return giftCardCodeGrouped(self.code.Secret)
}

func (self *GiftCardFriendTemplate) RedeemLink() string {
	if self.code == nil {
		return giftCardRedeemUrl
	}
	return giftCardRedeemLink(self.code.Secret)
}

func (self *GiftCardFriendTemplate) RedeemBy() string {
	if self.code == nil {
		return ""
	}
	return self.redeemBy(self.code)
}

// "Made with Bittensor" when the buyer chose it, or "".
func (self *GiftCardFriendTemplate) Bittensor() string {
	if self.order == nil || !self.order.BittensorFooter {
		return ""
	}
	return giftCardText(self.language(), "card.bittensor")
}

// The retention (§10.6): an order's PDFs, note, signature and friends'
// addresses are removed this long after its last email is sent.
const giftRetentionAfterLastEmail = 30 * 24 * time.Hour

// An email whose outbox row is gone (the outbox removes finished rows a week
// after they were added) was sent or given up by at most this long after it
// was due: its retries span about 15 hours (accountMessageMaxAttemptCount).
const giftRetentionEmailFinishBound = 24 * time.Hour

// The orders one query lists, and the longest one run reaps.
const (
	giftRetentionBatchSize = 500
	giftRetentionRunBudget = 5 * time.Minute
)

// The daily run, 04:15 UTC.
func NextGiftRetentionAt(now time.Time) time.Time {
	at := time.Date(now.Year(), now.Month(), now.Day(), 4, 15, 0, 0, time.UTC)
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return at
}

func ScheduleGiftRetention(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		GiftRetention,
		&GiftRetentionArgs{},
		clientSession,
		task.RunOnce("gift_retention"),
		task.RunAt(at),
		task.MaxTime(2*giftRetentionRunBudget),
	)
}

type GiftRetentionArgs struct {
}

type GiftRetentionResult struct {
	// orders whose personal data was removed
	RemovedCount int `json:"removed_count"`
	// orders listed whose last email is not 30 days old yet
	KeptCount int `json:"kept_count"`
	// orders never paid (pending, or refunded before payment) whose personal
	// data was removed
	UnpaidRemovedCount int `json:"unpaid_removed_count"`
	// crypto orders the 30-day unlink passed that forgot their buyer's
	// address, kept until no friend's email needed it
	UnlinkedBuyerEmailClearedCount int `json:"unlinked_buyer_email_cleared_count"`
}

// Removes the personal data of every order whose last email went out 30 days
// ago or more, and of every order never paid, 30 days after it was made or
// refunded; and the buyer's address of a crypto order the unlink passed once
// no friend's email needs it.
func GiftRetention(
	args *GiftRetentionArgs,
	clientSession *session.ClientSession,
) (*GiftRetentionResult, error) {
	return runGiftRetention(clientSession.Ctx, server.NowUtc())
}

// Schedules the next day's run.
func GiftRetentionPost(
	args *GiftRetentionArgs,
	result *GiftRetentionResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleGiftRetention(clientSession, tx, NextGiftRetentionAt(server.NowUtc()))
	return nil
}

func runGiftRetention(ctx context.Context, now time.Time) (*GiftRetentionResult, error) {
	result := &GiftRetentionResult{}
	endTime := time.Now().Add(giftRetentionRunBudget)
	cutoff := now.Add(-giftRetentionAfterLastEmail)
	running := func() bool {
		select {
		case <-ctx.Done():
			return false
		default:
			return time.Now().Before(endTime)
		}
	}

	// crypto orders the 30-day unlink passed (rail crypto, no invoice) that
	// kept their buyer's address as the friends' reply-to: the delivery
	// forgets it after the last friend's email, and this covers one it
	// missed. First, while the order's cards are still stored: an order of
	// delivery "them" without them reads as one whose emails are still owed.
	// Only orders made before the cutoff, as every unlinked one was: a crypto
	// order is made before its invoice is linked to it.
	var afterOrderId *server.Id
	for running() {
		giftOrderIds := listUnlinkedGiftOrderBuyerEmails(ctx, cutoff, afterOrderId, giftRetentionBatchSize)
		for _, giftOrderId := range giftOrderIds {
			if model.ClearUnlinkedGiftOrderBuyerEmail(ctx, giftOrderId) {
				result.UnlinkedBuyerEmailClearedCount += 1
			}
		}
		if len(giftOrderIds) < giftRetentionBatchSize {
			break
		}
		afterOrderId = &giftOrderIds[len(giftOrderIds)-1]
	}

	// paid orders, 30 days after their last email
	var after *giftRetentionCursor
	for running() {
		candidates := listGiftOrdersForRetention(ctx, cutoff, after, giftRetentionBatchSize)
		for _, candidate := range candidates {
			order := model.GetGiftOrder(ctx, candidate.giftOrderId)
			if order == nil {
				continue
			}
			lastEmailTime, finished := giftOrderLastEmailTime(ctx, order)
			if !finished || now.Before(lastEmailTime.Add(giftRetentionAfterLastEmail)) {
				result.KeptCount += 1
				continue
			}
			model.RemoveGiftOrderPersonalData(ctx, order.GiftOrderId)
			result.RemovedCount += 1
		}
		if len(candidates) < giftRetentionBatchSize {
			break
		}
		after = candidates[len(candidates)-1]
	}

	// orders never paid: 30 days after they were made, or after a refund
	// ended them
	after = nil
	for running() {
		candidates := listUnpaidGiftOrdersForRetention(ctx, cutoff, after, giftRetentionBatchSize)
		for _, candidate := range candidates {
			// an order paid since it was listed keeps its data (a payment in
			// the instant before the removal is delivered as a late one: the
			// buyer gets every card)
			order := model.GetGiftOrder(ctx, candidate.giftOrderId)
			if order == nil || order.PaidTime != nil {
				continue
			}
			switch order.Status {
			case model.GiftOrderStatusPending:
				model.RemoveGiftOrderPersonalData(ctx, order.GiftOrderId)
				if order.Rail == model.GiftRailStripe {
					// a Checkout session expires within a day: no late payment
					model.RemoveGiftOrderBuyerEmail(ctx, order.GiftOrderId)
				}
			case model.GiftOrderStatusRefunded:
				// refunded before it was ever paid: nothing needs the buyer
				model.RemoveGiftOrderPersonalData(ctx, order.GiftOrderId)
				model.RemoveGiftOrderBuyerEmail(ctx, order.GiftOrderId)
			default:
				continue
			}
			result.UnpaidRemovedCount += 1
		}
		if len(candidates) < giftRetentionBatchSize {
			break
		}
		after = candidates[len(candidates)-1]
	}

	if 0 < result.RemovedCount || 0 < result.UnpaidRemovedCount || 0 < result.UnlinkedBuyerEmailClearedCount {
		glog.Infof(
			"[gift]retention removed the cards and personal data of %d gift orders, the personal data of %d never paid, and the buyer's address of %d unlinked\n",
			result.RemovedCount,
			result.UnpaidRemovedCount,
			result.UnlinkedBuyerEmailClearedCount,
		)
	}
	return result, nil
}

// The crypto orders the 30-day unlink may have passed that still hold their
// buyer's address: rail crypto, no invoice, made before `cutoff`, by id after
// `afterOrderId`.
//
// TODO(part 1): this listing belongs in model/gift_order_model.go too.
func listUnlinkedGiftOrderBuyerEmails(ctx context.Context, cutoff time.Time, afterOrderId *server.Id, limit int) (giftOrderIds []server.Id) {
	afterId := server.Id{}
	if afterOrderId != nil {
		afterId = *afterOrderId
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT gift_order_id
				FROM gift_order
				WHERE
					rail = $1 AND
					crypto_intent_id IS NULL AND
					buyer_email <> '' AND
					create_time < $2 AND
					gift_order_id > $3
				ORDER BY gift_order_id
				LIMIT $4
			`,
			model.GiftRailCrypto,
			cutoff,
			afterId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var giftOrderId server.Id
				server.Raise(result.Scan(&giftOrderId))
				giftOrderIds = append(giftOrderIds, giftOrderId)
			}
		})
	})
	return
}

// An order the retention may reap, in (time, gift_order_id) order: the paid
// time of a paid order, the create time of an unpaid one.
type giftRetentionCursor struct {
	orderTime   time.Time
	giftOrderId server.Id
}

// The paid orders that still hold a PDF, a note, a signature or a friend's
// address, paid before `cutoff`, with no PDF stored since and no friend's date
// after it, in pages after `after`. An order paid, printed or sent to friends
// later cannot have sent its last email before the cutoff, so these are the
// only orders that can be due.
//
// TODO(part 1): this listing belongs in model/gift_order_model.go beside
// RemoveGiftOrderPersonalData.
func listGiftOrdersForRetention(ctx context.Context, cutoff time.Time, after *giftRetentionCursor, limit int) (candidates []*giftRetentionCursor) {
	afterTime := time.Time{}
	afterId := server.Id{}
	if after != nil {
		afterTime = after.orderTime
		afterId = after.giftOrderId
	}
	// a friend's email goes out at 9:00 on its date in any zone, at most 14
	// hours either side of the date in UTC
	sendOnCutoff := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					gift_order.paid_time,
					gift_order.gift_order_id
				FROM gift_order
				WHERE
					gift_order.paid_time IS NOT NULL AND
					gift_order.paid_time < $1 AND
					(gift_order.send_on IS NULL OR gift_order.send_on < $2 OR gift_order.status = $6) AND
					(gift_order.paid_time, gift_order.gift_order_id) > ($3, $4) AND
					NOT EXISTS (
						SELECT 1
						FROM gift_order_pdf
						WHERE
							gift_order_pdf.gift_order_id = gift_order.gift_order_id AND
							$1 <= gift_order_pdf.create_time
					) AND
					(
						gift_order.message <> '' OR
						gift_order.signature <> '' OR
						EXISTS (
							SELECT 1
							FROM gift_order_pdf
							WHERE gift_order_pdf.gift_order_id = gift_order.gift_order_id
						) OR
						EXISTS (
							SELECT 1
							FROM gift_order_recipient
							WHERE
								gift_order_recipient.gift_order_id = gift_order.gift_order_id AND
								gift_order_recipient.email <> ''
						)
					)
				ORDER BY gift_order.paid_time, gift_order.gift_order_id
				LIMIT $5
			`,
			cutoff,
			sendOnCutoff,
			afterTime,
			afterId,
			limit,
			model.GiftOrderStatusRefunded,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				candidate := &giftRetentionCursor{}
				server.Raise(result.Scan(&candidate.orderTime, &candidate.giftOrderId))
				candidates = append(candidates, candidate)
			}
		})
	})
	return
}

// The orders never paid that still hold personal data, in pages after
// `after`: pending ones made before `cutoff`, and ones a refund ended (before
// payment) before it. Personal data is a note, a signature or a friend's
// address, and for a pending card order or a refunded one its buyer's address
// too. A never-paid order has no card.
//
// TODO(part 1): this listing belongs in model/gift_order_model.go too.
func listUnpaidGiftOrdersForRetention(ctx context.Context, cutoff time.Time, after *giftRetentionCursor, limit int) (candidates []*giftRetentionCursor) {
	afterTime := time.Time{}
	afterId := server.Id{}
	if after != nil {
		afterTime = after.orderTime
		afterId = after.giftOrderId
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					gift_order.create_time,
					gift_order.gift_order_id
				FROM gift_order
				WHERE
					gift_order.paid_time IS NULL AND
					(
						(gift_order.status = $1 AND gift_order.create_time < $3) OR
						(gift_order.status = $2 AND COALESCE(gift_order.refunded_time, gift_order.create_time) < $3)
					) AND
					(gift_order.create_time, gift_order.gift_order_id) > ($4, $5) AND
					(
						gift_order.message <> '' OR
						gift_order.signature <> '' OR
						(gift_order.status = $1 AND gift_order.rail = $6 AND gift_order.buyer_email <> '') OR
						(gift_order.status = $2 AND gift_order.buyer_email <> '') OR
						EXISTS (
							SELECT 1
							FROM gift_order_recipient
							WHERE
								gift_order_recipient.gift_order_id = gift_order.gift_order_id AND
								gift_order_recipient.email <> ''
						)
					)
				ORDER BY gift_order.create_time, gift_order.gift_order_id
				LIMIT $7
			`,
			model.GiftOrderStatusPending,
			model.GiftOrderStatusRefunded,
			cutoff,
			afterTime,
			afterId,
			model.GiftRailStripe,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				candidate := &giftRetentionCursor{}
				server.Raise(result.Scan(&candidate.orderTime, &candidate.giftOrderId))
				candidates = append(candidates, candidate)
			}
		})
	})
	return
}

// When the order's last email went out, and whether every one did. An email
// still due, or being retried, is not finished. One whose outbox row is gone
// finished by giftRetentionEmailFinishBound after it was due. An order that was
// never printed sent nothing, and is not finished either.
func giftOrderLastEmailTime(ctx context.Context, order *model.GiftOrder) (lastEmailTime time.Time, finished bool) {
	if order.PaidTime == nil {
		return time.Time{}, false
	}
	type owedEmail struct {
		templateName string
		key          string
		dueTime      time.Time
	}
	emails := []owedEmail{{
		templateName: giftCardsBuyerTemplateName,
		key:          giftBuyerEmailKey(order.GiftOrderId),
		dueTime:      *order.PaidTime,
	}}
	if order.Delivery == model.GiftDeliveryThem {
		friendDueTime := giftFriendDeliverTime(order, *order.PaidTime)
		for recipientIndex := range order.Quantity {
			emails = append(emails, owedEmail{
				templateName: giftCardFriendTemplateName,
				key:          giftFriendEmailKey(order.GiftOrderId, recipientIndex),
				dueTime:      friendDueTime,
			})
		}
	}
	foundCount := 0
	for _, email := range emails {
		message := model.GetAccountMessageByKey(ctx, email.templateName, email.key)
		var finishTime time.Time
		switch {
		case message == nil:
			finishTime = email.dueTime.Add(giftRetentionEmailFinishBound)
		case message.SentTime != nil:
			finishTime = *message.SentTime
			foundCount += 1
		case message.AbandonTime != nil:
			finishTime = *message.AbandonTime
			foundCount += 1
		default:
			return time.Time{}, false
		}
		if lastEmailTime.Before(finishTime) {
			lastEmailTime = finishTime
		}
	}
	if foundCount == 0 && !order.PdfReady {
		if order.Status == model.GiftOrderStatusRefunded {
			// refunded before its cards were printed: no email was ever owed
			// (the render prints nothing for an order no longer paid), so
			// the clock runs from the refund
			refundTime := *order.PaidTime
			if order.RefundedTime != nil {
				refundTime = *order.RefundedTime
			}
			return refundTime, true
		}
		// paid, its cards not printed yet: its emails are still owed
		return time.Time{}, false
	}
	return lastEmailTime, true
}
