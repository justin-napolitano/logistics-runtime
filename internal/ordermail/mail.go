// Package ordermail extracts bounded, untrusted order reports without network or model calls.
package ordermail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxMessageBytes = 2 << 20
const ParserVersion = "amazon-en-v1"

var ErrInvalid = errors.New("invalid mail input")

type Event struct {
	OrderReference string    `json:"order_reference"`
	Kind           string    `json:"kind"`
	OccurredAt     time.Time `json:"occurred_at"`
	TrackingNumber string    `json:"tracking_number"`
	TotalMinor     *int64    `json:"total_minor"`
	Currency       string    `json:"currency"`
	Trust          string    `json:"trust"`
}
type Message struct {
	MessageKey    string `json:"message_key"`
	Digest        string `json:"digest"`
	ParserVersion string `json:"parser_version"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	Event         *Event `json:"event"`
}
type ImportResult struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Duplicate bool   `json:"duplicate"`
	OrderID   string `json:"order_id"`
}
type Evidence struct {
	ID            string    `json:"id"`
	Account       string    `json:"account"`
	MessageKey    string    `json:"message_key"`
	ParserVersion string    `json:"parser_version"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason"`
	ObservedAt    time.Time `json:"observed_at"`
}
type Report struct {
	ID string `json:"id"`
	Event
	ObservedAt time.Time `json:"observed_at"`
}
type Order struct {
	ID        string   `json:"id"`
	Account   string   `json:"account"`
	Merchant  string   `json:"merchant"`
	Reference string   `json:"reference"`
	Coverage  string   `json:"coverage"`
	Reports   []Report `json:"reports"`
}
type Store interface {
	ImportMail(context.Context, string, Message) (ImportResult, error)
	Orders(context.Context, int, int) ([]Order, error)
	Order(context.Context, string) (Order, error)
	MailExceptions(context.Context, int, int) ([]Evidence, error)
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func OrderID(account, reference string) string {
	return digest([]byte(account + "\x00amazon\x00" + reference))
}

// Parse retains only extracted facts and evidence identity, not the message body.
// Sender checks are routing hints, not proof of sender authenticity or purchase authority.
func Parse(r io.Reader) (Message, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxMessageBytes+1))
	if err != nil || len(raw) > MaxMessageBytes {
		return Message{}, ErrInvalid
	}
	out := Message{Digest: digest(raw), ParserVersion: ParserVersion, Status: "exception", Reason: "malformed_message"}
	out.MessageKey = "sha256:" + out.Digest
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return out, nil
	}
	if key := strings.TrimSpace(m.Header.Get("Message-ID")); key != "" {
		if len(key) > 200 || strings.ContainsAny(key, "\r\n\x00") {
			out.Reason = "invalid_message_identity"
			return out, nil
		}
		out.MessageKey = key
	}
	from, err := mail.ParseAddress(m.Header.Get("From"))
	if err != nil {
		out.Reason = "unsupported_sender"
		return out, nil
	}
	parts := strings.Split(strings.ToLower(from.Address), "@")
	if len(parts) != 2 || !(parts[1] == "amazon.com" || strings.HasSuffix(parts[1], ".amazon.com")) {
		out.Reason = "unsupported_sender"
		return out, nil
	}
	date, err := m.Header.Date()
	if err != nil {
		out.Reason = "missing_or_invalid_event_date"
		return out, nil
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		out.Reason = "unsupported_subject_encoding"
		return out, nil
	}
	budget := 128
	text, err := bodyText(m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), m.Body, 0, &budget)
	if err != nil {
		out.Reason = "unsupported_or_malformed_body"
		return out, nil
	}
	text = subject + "\n" + text
	refs := orderPattern.FindAllString(text, -1)
	unique := map[string]bool{}
	for _, v := range refs {
		unique[v] = true
	}
	if len(unique) != 1 {
		out.Reason = "missing_or_ambiguous_order_reference"
		return out, nil
	}
	reference := ""
	for v := range unique {
		reference = v
	}
	lower := strings.ToLower(subject)
	kind := ""
	switch {
	case strings.Contains(lower, "refund"):
		kind = "refund_reported"
	case strings.Contains(lower, "cancelled") || strings.Contains(lower, "canceled"):
		kind = "cancellation_reported"
	case strings.Contains(lower, "delivered"):
		kind = "delivered_reported"
	case strings.Contains(lower, "shipped") || strings.Contains(lower, "dispatched") || strings.Contains(lower, "on the way"):
		kind = "shipped_reported"
	case strings.Contains(lower, "delivery update"):
		kind = "delivery_update_reported"
	case strings.Contains(lower, "order confirmation") || strings.Contains(lower, "ordered:") || strings.Contains(lower, "your amazon.com order") || strings.Contains(lower, "your amazon fresh order"):
		kind = "confirmation_reported"
	default:
		out.Reason = "unsupported_subject_template"
		return out, nil
	}
	e := Event{OrderReference: reference, Kind: kind, OccurredAt: date.UTC(), Trust: "unverified_email_report"}
	// Multiple tracking numbers cannot safely be assigned to one shipment here.
	tracks := trackingPattern.FindAllStringSubmatch(text, -1)
	seen := map[string]bool{}
	for _, t := range tracks {
		seen[t[1]] = true
	}
	if len(seen) == 1 {
		for v := range seen {
			e.TrackingNumber = v
		}
	}
	totals := totalPattern.FindAllStringSubmatch(text, -1)
	money := map[int64]bool{}
	for _, m := range totals {
		whole, er := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
		if er == nil && whole <= 1000000000 {
			fraction, _ := strconv.ParseInt(m[2], 10, 64)
			money[whole*100+fraction] = true
		}
	}
	if len(money) == 1 {
		for v := range money {
			n := v
			e.TotalMinor = &n
			e.Currency = "USD"
		}
	}
	out.Status = "parsed"
	out.Reason = ""
	out.Event = &e
	return out, nil
}

var orderPattern = regexp.MustCompile(`\b[0-9]{3}-[0-9]{7}-[0-9]{7}\b`)
var trackingPattern = regexp.MustCompile(`(?im)\btracking\s+(?:number|id)\s*:\s*([A-Za-z0-9-]{6,80})\b`)
var totalPattern = regexp.MustCompile(`(?i)\border total\s*:\s*(?:USD\s*)?\$([0-9][0-9,]{0,13})\.([0-9]{2})\b`)
var scripts = regexp.MustCompile(`(?is)<(?:script|style)\b[^>]*>.*?</(?:script|style)\s*>`)
var tags = regexp.MustCompile(`(?s)<[^>]*>`)

func bodyText(contentType, encoding string, r io.Reader, depth int, budget *int) (string, error) {
	*budget--
	if depth > 10 || *budget < 0 {
		return "", ErrInvalid
	}
	if contentType == "" {
		contentType = "text/plain"
	}
	typ, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "", "7bit", "8bit", "binary":
	default:
		return "", ErrInvalid
	}
	if strings.HasPrefix(typ, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", ErrInvalid
		}
		reader := multipart.NewReader(r, boundary)
		var texts []string
		for {
			p, er := reader.NextPart()
			if er == io.EOF {
				break
			}
			if er != nil {
				return "", er
			}
			*budget--
			if *budget < 0 {
				p.Close()
				return "", ErrInvalid
			}
			disp, _, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
			if disp == "attachment" || p.FileName() != "" {
				p.Close()
				continue
			}
			t, er := bodyText(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, depth+1, budget)
			p.Close()
			if er != nil {
				return "", er
			}
			texts = append(texts, t)
		}
		return strings.Join(texts, "\n"), nil
	}
	if typ != "text/plain" && typ != "text/html" {
		return "", nil
	}
	if charset := strings.ToLower(params["charset"]); charset != "" && charset != "utf-8" && charset != "us-ascii" {
		return "", ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxMessageBytes+1))
	if err != nil || len(data) > MaxMessageBytes || !utf8.Valid(data) {
		return "", ErrInvalid
	}
	text := string(data)
	if typ == "text/html" {
		text = html.UnescapeString(tags.ReplaceAllString(scripts.ReplaceAllString(text, " "), " "))
	}
	return text, nil
}
