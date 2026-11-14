package controller

// The raw MIME of an email that carries files (AttachmentTemplate), for SES
// SendRawEmail: a multipart/mixed message whose first part is the
// multipart/alternative body (text, then html, both quoted-printable) and whose
// other parts are the files, base64. Every other email goes through SendEmail,
// which writes its own MIME.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// SES refuses a raw message larger than 10 MB, counted after encoding.
const rawEmailMaxByteCount = 10 * 1024 * 1024

// The length of a base64 line in a MIME part (RFC 2045).
const rawEmailBase64LineLength = 76

// One email, ready to be written as MIME.
type rawEmail struct {
	from        string
	to          string
	replyTo     string
	subject     string
	bodyText    string
	bodyHtml    string
	attachments []*EmailAttachment
	date        time.Time
}

// The value as one header address: a single mailbox with no line break,
// formatted for a header. ok is false for anything else.
func emailHeaderAddress(value string) (address string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil {
		return "", false
	}
	if parsed.Name == "" {
		return parsed.Address, true
	}
	return parsed.String(), true
}

// Writes the email as MIME. Every header value is checked or encoded, so text
// from a template (a buyer's signature in a subject) cannot add a header.
func buildRawEmail(email *rawEmail) ([]byte, error) {
	from, ok := emailHeaderAddress(email.from)
	if !ok {
		return nil, errors.New("raw email: invalid sender address")
	}
	to, ok := emailHeaderAddress(email.to)
	if !ok {
		return nil, errors.New("raw email: invalid recipient address")
	}
	replyTo := ""
	if email.replyTo != "" {
		if replyTo, ok = emailHeaderAddress(email.replyTo); !ok {
			return nil, errors.New("raw email: invalid reply-to address")
		}
	}
	if email.subject == "" || strings.ContainsAny(email.subject, "\r\n") {
		return nil, errors.New("raw email: the subject must be one line")
	}

	out := &bytes.Buffer{}
	mixed := multipart.NewWriter(out)
	writeHeader := func(name string, value string) {
		fmt.Fprintf(out, "%s: %s\r\n", name, value)
	}
	writeHeader("From", from)
	writeHeader("To", to)
	if replyTo != "" {
		writeHeader("Reply-To", replyTo)
	}
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", email.subject))
	writeHeader("Date", email.date.Format(time.RFC1123Z))
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": mixed.Boundary()}))
	out.WriteString("\r\n")

	// the body: text, then html, the preferred last (RFC 2046 §5.1.4)
	body := &bytes.Buffer{}
	alternative := multipart.NewWriter(body)
	for _, bodyPart := range []struct {
		contentType string
		content     string
	}{
		{"text/plain; charset=UTF-8", email.bodyText},
		{"text/html; charset=UTF-8", email.bodyHtml},
	} {
		partWriter, err := alternative.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {bodyPart.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		encoder := quotedprintable.NewWriter(partWriter)
		if _, err := io.WriteString(encoder, bodyPart.content); err != nil {
			return nil, err
		}
		if err := encoder.Close(); err != nil {
			return nil, err
		}
	}
	if err := alternative.Close(); err != nil {
		return nil, err
	}
	bodyWriter, err := mixed.CreatePart(textproto.MIMEHeader{
		"Content-Type": {mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": alternative.Boundary()})},
	})
	if err != nil {
		return nil, err
	}
	if _, err := bodyWriter.Write(body.Bytes()); err != nil {
		return nil, err
	}

	for _, attachment := range email.attachments {
		filename := attachment.Filename
		if filename == "" || strings.ContainsAny(filename, "\r\n\"/\\") {
			return nil, fmt.Errorf("raw email: invalid attachment name %q", filename)
		}
		contentType := attachment.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return nil, fmt.Errorf("raw email: attachment %s: %w", filename, err)
		}
		partWriter, err := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {mime.FormatMediaType(mediaType, map[string]string{"name": filename})},
			"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": filename})},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeBase64Lines(partWriter, attachment.Data); err != nil {
			return nil, err
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, err
	}
	if rawEmailMaxByteCount < out.Len() {
		return nil, fmt.Errorf("raw email: %d bytes, over the %d SES accepts", out.Len(), rawEmailMaxByteCount)
	}
	return out.Bytes(), nil
}

// Writes the data as base64 in lines of 76 characters.
func writeBase64Lines(w io.Writer, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	for 0 < len(encoded) {
		n := min(rawEmailBase64LineLength, len(encoded))
		if _, err := io.WriteString(w, encoded[:n]+"\r\n"); err != nil {
			return err
		}
		encoded = encoded[n:]
	}
	return nil
}
