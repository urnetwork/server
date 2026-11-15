package controller

// The seal on §21.10's off-site records (QD-726). Every record the abuse
// mailbox or the legal-request mailbox receives leaves the process sealed, so
// the mailbox provider and SES see only its sender, time and size (QD-030,
// owner decision 208), and only the offline private key opens it, at §19.10's
// restore. HPKE-XWing as smtpedge's sealed report copies are (QD-441): KEM
// 0x647a, KDF HKDF-SHA256, AEAD AES-128-GCM, base mode, through crypto/hpke's
// Sender:
//
//	sealed  u8(1) || enc (1,120 B) || ct
//	ct      one Seal of the record's bytes, with no aad: its canonical JSON with
//	        its MAC inside (QD-078), so the MAC is as hidden as the record
//	info    "UR abuse 1.0 record"
//
// The mail is one text/plain body, the sealed bytes in standard base64 in lines
// of at most BodyLineCharCount characters (RFC 2045's 76), under a subject that
// never changes, through api's and taskworker's SES path. A restore reads a
// body back with DecodeAbuseMailboxBody and opens it with
// OpenAbuseMailboxRecord, kept beside the sealer so the two cannot drift.
//
// Both mailboxes take their keys from vault's abuse-mailbox.yml, read by name
// through server's vault loader on every record, so a file the owner adds or
// replaces takes effect at the next one. api and taskworker mount it, and only
// they install the production mailer (InstallAbuseRecordMailer):
//
//	abuse_mailbox_mac_key: <base64 of 32 random bytes>       # QD-078's HMAC-SHA-256 key
//	abuse_mailbox_seal_public_key: <base64 of 1,216 bytes>   # the X-Wing public key
//
// The file holds those two keys and nothing else. A file with any other key,
// a private key above all, is refused as a missing or malformed one is, and
// every record then stays owed (mailed_time NULL) until a good file arrives.
// The private key is held offline beside §19.10's restore runbook and is
// reachable by no running service; the owner creates the file. No error names
// a key's bytes.
//
// The seal's one ephemeral key is drawn inside crypto/hpke; nothing here
// creates, derives or unwraps a key of its own.

import (
	"context"
	"crypto/hpke"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ses"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

const (
	// The vault file of both mailboxes' keys (QD-726).
	AbuseMailboxKeysResourceName = "abuse-mailbox.yml"
	// The file's two keys.
	abuseMailboxMacKeyName        = "abuse_mailbox_mac_key"
	abuseMailboxSealPublicKeyName = "abuse_mailbox_seal_public_key"

	// The sealed form's version, its first byte.
	abuseMailboxSealVersion = 1
	abuseMailboxSealInfo    = "UR abuse 1.0 record"
	// X-Wing's encapsulation: ML-KEM-768's ciphertext and an X25519 share.
	abuseMailboxSealEncapsulationByteCount = 1120
	// AES-128-GCM's tag.
	abuseMailboxSealTagByteCount = 16

	// Every mail's subject, the same for every record, so it says nothing of one.
	abuseMailboxMailSubject = "URnetwork sealed record"
	// The template tag SES's events carry, beside email.yml's configuration set.
	abuseMailboxSesTemplateName = "abuse_mailbox_record"
)

// The key file is missing, unreadable or refused: it does not parse, holds a
// key other than its two, or a key that is the wrong size or not a key. A
// mailer that meets it sends nothing, and every record stays owed.
var ErrAbuseMailboxKeys = errors.New("abuse mailbox keys unavailable")

// No record can go to a mailbox now: the process has no email.yml, or one that
// names no sender. The record stays owed.
var ErrAbuseMailboxUnavailable = errors.New("abuse mailbox unavailable")

// A record that cannot be sealed: past MaxSealedByteCount, or a key not of the
// seal's suite.
var ErrAbuseMailboxSeal = errors.New("abuse mailbox record cannot be sealed")

// A sealed record that does not open: another key, changed bytes, or bytes no
// sealer wrote.
var ErrAbuseMailboxSealOpen = errors.New("abuse mailbox record does not open")

// Tunables of the mailboxes' records. The first three are the format's: a
// restore reads what a mailer wrote, so changing one is a format change, made
// on both sides at once.
type AbuseMailboxSettings struct {
	// The MAC key's length in bytes: the 32 random bytes QD-726 names,
	// HMAC-SHA-256's output length. A key file holding any other length is
	// refused.
	MacKeyByteCount int
	// The most base64 characters on one line of a mail's body: 76, RFC 2045's
	// limit for base64, so no line nears RFC 5322's 998 whatever transfer
	// encoding SES picks. Non-positive writes the body on one line.
	BodyLineCharCount int
	// The most bytes of one sealed record, the version byte, encapsulation and
	// tag included: 64 KiB, far past any record (an action's is under 2 KiB),
	// so a sealer refuses runaway input and a restore refuses a body no mailer
	// wrote before decoding all of it.
	MaxSealedByteCount int
	// The region of the SES identity the account sender sends from, as
	// aws_controller.go's sendEmail uses: us-west-1.
	SesRegion string
}

// The format's values and the account sender's region.
func DefaultAbuseMailboxSettings() *AbuseMailboxSettings {
	return &AbuseMailboxSettings{
		MacKeyByteCount:    32,
		BodyLineCharCount:  76,
		MaxSealedByteCount: 64 * 1024,
		SesRegion:          "us-west-1",
	}
}

// Both mailboxes' keys, as abuse-mailbox.yml holds them.
type AbuseMailboxKeys struct {
	// QD-078's HMAC-SHA-256 key, MacKeyByteCount bytes
	MacKey []byte
	// the X-Wing public key every record is sealed to
	SealPublicKey hpke.PublicKey
}

// Reads abuse-mailbox.yml's bytes: exactly its two keys, each in standard
// base64 (whitespace inside ignored, as a folded YAML scalar leaves it), the
// MAC key MacKeyByteCount bytes and not all zero, the seal key an X-Wing
// public key of 1,216 bytes. Anything else is refused with
// ErrAbuseMailboxKeys, naming the part and never a value: not even an unknown
// key's name, which a misplaced value could be.
func ParseAbuseMailboxKeys(data []byte, settings *AbuseMailboxSettings) (*AbuseMailboxKeys, error) {
	keyValues := map[string]any{}
	if err := yaml.Unmarshal(data, &keyValues); err != nil {
		return nil, fmt.Errorf("%w: %s does not parse", ErrAbuseMailboxKeys, AbuseMailboxKeysResourceName)
	}
	for key := range keyValues {
		if key != abuseMailboxMacKeyName && key != abuseMailboxSealPublicKeyName {
			return nil, fmt.Errorf(
				"%w: %s holds a key other than %s and %s, and the seal's private key stays offline",
				ErrAbuseMailboxKeys,
				AbuseMailboxKeysResourceName,
				abuseMailboxMacKeyName,
				abuseMailboxSealPublicKeyName,
			)
		}
	}
	decode := func(name string) ([]byte, error) {
		value, ok := keyValues[name].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%w: %s has no %s", ErrAbuseMailboxKeys, AbuseMailboxKeysResourceName, name)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(value), ""))
		if err != nil {
			return nil, fmt.Errorf("%w: %s's %s is not standard base64", ErrAbuseMailboxKeys, AbuseMailboxKeysResourceName, name)
		}
		return decoded, nil
	}

	macKey, err := decode(abuseMailboxMacKeyName)
	if err != nil {
		return nil, err
	}
	if len(macKey) != settings.MacKeyByteCount {
		return nil, fmt.Errorf(
			"%w: %s's %s is not %d bytes",
			ErrAbuseMailboxKeys,
			AbuseMailboxKeysResourceName,
			abuseMailboxMacKeyName,
			settings.MacKeyByteCount,
		)
	}
	zero := true
	for _, b := range macKey {
		if b != 0 {
			zero = false
			break
		}
	}
	if zero {
		// a placeholder, not 32 random bytes
		return nil, fmt.Errorf("%w: %s's %s is all zero", ErrAbuseMailboxKeys, AbuseMailboxKeysResourceName, abuseMailboxMacKeyName)
	}

	sealPublicKeyBytes, err := decode(abuseMailboxSealPublicKeyName)
	if err != nil {
		return nil, err
	}
	sealPublicKey, err := hpke.MLKEM768X25519().NewPublicKey(sealPublicKeyBytes)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: %s's %s is not an X-Wing public key",
			ErrAbuseMailboxKeys,
			AbuseMailboxKeysResourceName,
			abuseMailboxSealPublicKeyName,
		)
	}
	return &AbuseMailboxKeys{
		MacKey:        macKey,
		SealPublicKey: sealPublicKey,
	}, nil
}

// abuse-mailbox.yml's bytes for keys, as ParseAbuseMailboxKeys reads them:
// what a key generator writes for the owner to place in vault. The file has no
// place for a private key.
func AbuseMailboxKeyFile(keys *AbuseMailboxKeys) []byte {
	return []byte(abuseMailboxMacKeyName + ": " + base64.StdEncoding.EncodeToString(keys.MacKey) + "\n" +
		abuseMailboxSealPublicKeyName + ": " + base64.StdEncoding.EncodeToString(keys.SealPublicKey.Bytes()) + "\n")
}

// The keys from this process's vault, read by name on each call. A process
// that does not mount the file answers ErrAbuseMailboxKeys.
func LoadAbuseMailboxKeys(settings *AbuseMailboxSettings) (*AbuseMailboxKeys, error) {
	resource, err := server.Vault.SimpleResource(AbuseMailboxKeysResourceName)
	if err != nil {
		return nil, fmt.Errorf("%w: no %s: %v", ErrAbuseMailboxKeys, AbuseMailboxKeysResourceName, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrAbuseMailboxKeys, AbuseMailboxKeysResourceName, err)
	}
	return ParseAbuseMailboxKeys(data, settings)
}

// Seals plaintext to publicKey: u8(1) || enc || ct, as the file header writes.
// Each call draws a fresh encapsulation, so two seals of one record differ.
func SealAbuseMailboxRecord(publicKey hpke.PublicKey, plaintext []byte, settings *AbuseMailboxSettings) ([]byte, error) {
	if publicKey == nil || publicKey.KEM().ID() != hpke.MLKEM768X25519().ID() {
		return nil, fmt.Errorf("%w: the key is not an X-Wing public key", ErrAbuseMailboxSeal)
	}
	sealedByteCount := 1 + abuseMailboxSealEncapsulationByteCount + len(plaintext) + abuseMailboxSealTagByteCount
	if settings.MaxSealedByteCount < sealedByteCount {
		return nil, fmt.Errorf("%w: %d bytes sealed, past %d", ErrAbuseMailboxSeal, sealedByteCount, settings.MaxSealedByteCount)
	}
	encapsulation, sender, err := hpke.NewSender(publicKey, hpke.HKDFSHA256(), hpke.AES128GCM(), []byte(abuseMailboxSealInfo))
	if err != nil {
		return nil, fmt.Errorf("%w: hpke setup: %v", ErrAbuseMailboxSeal, err)
	}
	if len(encapsulation) != abuseMailboxSealEncapsulationByteCount {
		return nil, fmt.Errorf("%w: an encapsulation of %d bytes", ErrAbuseMailboxSeal, len(encapsulation))
	}
	ciphertext, err := sender.Seal(nil, plaintext)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAbuseMailboxSeal, err)
	}
	sealed := make([]byte, 0, sealedByteCount)
	sealed = append(sealed, abuseMailboxSealVersion)
	sealed = append(sealed, encapsulation...)
	sealed = append(sealed, ciphertext...)
	return sealed, nil
}

// A sealed record's bytes, opened with the seal's private key: the format's
// other half, for §19.10's restore, which holds the key offline. Any failure
// is ErrAbuseMailboxSealOpen, saying nothing of which part failed.
func OpenAbuseMailboxRecord(privateKey hpke.PrivateKey, sealed []byte, settings *AbuseMailboxSettings) ([]byte, error) {
	if privateKey == nil || privateKey.KEM().ID() != hpke.MLKEM768X25519().ID() {
		return nil, ErrAbuseMailboxSealOpen
	}
	if len(sealed) < 1+abuseMailboxSealEncapsulationByteCount+abuseMailboxSealTagByteCount ||
		settings.MaxSealedByteCount < len(sealed) ||
		sealed[0] != abuseMailboxSealVersion {
		return nil, ErrAbuseMailboxSealOpen
	}
	recipient, err := hpke.NewRecipient(
		sealed[1:1+abuseMailboxSealEncapsulationByteCount],
		privateKey,
		hpke.HKDFSHA256(),
		hpke.AES128GCM(),
		[]byte(abuseMailboxSealInfo),
	)
	if err != nil {
		return nil, ErrAbuseMailboxSealOpen
	}
	plaintext, err := recipient.Open(nil, sealed[1+abuseMailboxSealEncapsulationByteCount:])
	if err != nil {
		return nil, ErrAbuseMailboxSealOpen
	}
	return plaintext, nil
}

// A mail's text/plain body for sealed: standard base64 in lines of at most
// BodyLineCharCount characters, each ending in a newline.
func EncodeAbuseMailboxBody(sealed []byte, settings *AbuseMailboxSettings) string {
	encoded := base64.StdEncoding.EncodeToString(sealed)
	if settings.BodyLineCharCount <= 0 {
		return encoded + "\n"
	}
	body := &strings.Builder{}
	body.Grow(len(encoded) + len(encoded)/settings.BodyLineCharCount + 1)
	for 0 < len(encoded) {
		take := min(settings.BodyLineCharCount, len(encoded))
		body.WriteString(encoded[:take])
		body.WriteString("\n")
		encoded = encoded[take:]
	}
	return body.String()
}

// The sealed bytes of a mail's text/plain body, whitespace and line breaks
// ignored, as a mailbox's export or a transfer encoding may rewrap them. A body
// that is not base64, or decodes past MaxSealedByteCount, is
// ErrAbuseMailboxSealOpen.
func DecodeAbuseMailboxBody(body string, settings *AbuseMailboxSettings) ([]byte, error) {
	compact := strings.Join(strings.Fields(body), "")
	if settings.MaxSealedByteCount < base64.StdEncoding.DecodedLen(len(compact)) {
		return nil, ErrAbuseMailboxSealOpen
	}
	sealed, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, ErrAbuseMailboxSealOpen
	}
	return sealed, nil
}

// Where a sealed record's mail goes: one text/plain mail to a mailbox. Safe
// for concurrent use.
type abuseMailboxSender interface {
	sendAbuseMailboxMail(ctx context.Context, mailbox string, subject string, bodyText string) error
}

// Seals plaintext, a record's bytes with its MAC inside, to keys' seal key,
// and mails the body to mailbox through sender: what both mailboxes' mailers
// do once they hold a record's bytes. Nothing but the sealed body and the
// fixed subject reaches sender. An error wraps ErrAbuseMailboxUnavailable when
// no record can go now; any other is this record's failed seal or send.
func mailSealedAbuseMailboxRecord(
	ctx context.Context,
	sender abuseMailboxSender,
	mailbox string,
	plaintext []byte,
	keys *AbuseMailboxKeys,
	settings *AbuseMailboxSettings,
) error {
	sealed, err := SealAbuseMailboxRecord(keys.SealPublicKey, plaintext, settings)
	if err != nil {
		return err
	}
	return sender.sendAbuseMailboxMail(ctx, mailbox, abuseMailboxMailSubject, EncodeAbuseMailboxBody(sealed, settings))
}

// api's and taskworker's SES path (aws_controller.go): the account sender's
// credentials, endpoint and send timeout, and email.yml's sender, reply-to and
// configuration set, with one text/plain body and no html part. The send also
// ends with the caller's ctx. The zero value sends as the production account
// sender does.
type sesAbuseMailboxSender struct {
	// nil sends as the zero AWSMessageSender does
	sender *AWSMessageSender
	// nil reads email.yml (EnvEmailConfig); a process without one sends nothing
	emailConfig *EmailConfig
	// nil is DefaultAbuseMailboxSettings
	settings *AbuseMailboxSettings
}

// One SES SendEmail call, bounded by the send timeout and ctx. A process with
// no email.yml, or one naming no sender, answers ErrAbuseMailboxUnavailable;
// a call that runs out of time returns an error that is
// context.DeadlineExceeded, and one whose ctx ends, that context's error.
func (self *sesAbuseMailboxSender) sendAbuseMailboxMail(ctx context.Context, mailbox string, subject string, bodyText string) error {
	settings := self.settings
	if settings == nil {
		settings = DefaultAbuseMailboxSettings()
	}
	sender := self.sender
	if sender == nil {
		sender = &AWSMessageSender{}
	}
	emailConfig := self.emailConfig
	if emailConfig == nil {
		// EnvEmailConfig panics in a process with no email.yml
		if _, err := server.Config.SimpleResource("email.yml"); err != nil {
			return fmt.Errorf("%w: no email.yml", ErrAbuseMailboxUnavailable)
		}
		emailConfig = EnvEmailConfig()
	}
	senderEmail := strings.TrimSpace(emailConfig.CompanySenderEmail)
	if senderEmail == "" {
		return fmt.Errorf("%w: email.yml names no company_sender_email", ErrAbuseMailboxUnavailable)
	}
	sendTimeout := emailConfig.SendTimeout()
	if 0 < sender.sendTimeout {
		sendTimeout = sender.sendTimeout
	}
	sendCtx, sendCancel := context.WithTimeout(ctx, sendTimeout)
	defer sendCancel()

	awsSession, err := sender.newSession(settings.SesRegion, sendTimeout)
	if err != nil {
		return fmt.Errorf("abuse mailbox mail: %w", err)
	}
	charSet := "UTF-8"
	input := &ses.SendEmailInput{
		Destination: &ses.Destination{
			ToAddresses: []*string{aws.String(mailbox)},
		},
		Message: &ses.Message{
			Body: &ses.Body{
				Text: &ses.Content{
					Charset: aws.String(charSet),
					Data:    aws.String(bodyText),
				},
			},
			Subject: &ses.Content{
				Charset: aws.String(charSet),
				Data:    aws.String(subject),
			},
		},
		Source: aws.String(senderEmail),
	}
	if replyTo := strings.TrimSpace(emailConfig.ReplyToEmail); replyTo != "" {
		input.ReplyToAddresses = []*string{aws.String(replyTo)}
	}
	if configurationSet := strings.TrimSpace(emailConfig.ConfigurationSet); configurationSet != "" {
		input.ConfigurationSetName = aws.String(configurationSet)
		input.Tags = []*ses.MessageTag{{
			Name:  aws.String("template"),
			Value: aws.String(abuseMailboxSesTemplateName),
		}}
	}
	// the call and every SDK retry inside it stop at the send timeout or ctx
	if _, err := ses.New(awsSession).SendEmailWithContext(sendCtx, input); err != nil {
		if ctxErr := sendCtx.Err(); ctxErr != nil {
			// the SDK's errors do not unwrap to the context's
			return fmt.Errorf("abuse mailbox mail stopped within %s: %w: %w", sendTimeout, ctxErr, err)
		}
		return fmt.Errorf("abuse mailbox mail: %w", err)
	}
	return nil
}
