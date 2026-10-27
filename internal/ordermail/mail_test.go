package ordermail

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func message(subject, body string) string {
	return fmt.Sprintf("From: auto-confirm@amazon.com\r\nMessage-ID: <synthetic-one@example.invalid>\r\nDate: Tue, 27 Oct 2026 10:00:00 -0400\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s", subject, body)
}
func TestAmazonEvidence(t *testing.T) {
	for _, tc := range []struct{ subject, kind string }{{"Your Amazon Fresh order", "confirmation_reported"}, {"Shipped: Amazon order", "shipped_reported"}, {"Delivered: Amazon order", "delivered_reported"}, {"Your order was canceled", "cancellation_reported"}, {"Refund issued", "refund_reported"}, {"Delivery update", "delivery_update_reported"}} {
		raw := message(tc.subject, "Order 111-1234567-1234567\nOrder total: $1,234.56\nTracking number: TBA123456789012\n")
		out, err := Parse(strings.NewReader(raw))
		if err != nil || out.Status != "parsed" {
			t.Fatalf("%s: %+v %v", tc.subject, out, err)
		}
		if out.Event.Kind != tc.kind || out.Event.TotalMinor == nil || *out.Event.TotalMinor != 123456 || out.Event.Currency != "USD" || out.Event.TrackingNumber != "TBA123456789012" || out.Event.Trust != "unverified_email_report" {
			t.Fatalf("facts %+v", out.Event)
		}
		if out.Event.OccurredAt.Hour() != 14 {
			t.Fatal("event timezone discarded")
		}
	}
}
func TestUnknownAndAmbiguousEvidence(t *testing.T) {
	tests := []struct{ raw, reason string }{
		{message("Advertisement", "111-1234567-1234567"), "unsupported_subject_template"},
		{message("Your Amazon Fresh order", "No reference"), "missing_or_ambiguous_order_reference"},
		{message("Your Amazon Fresh order", "111-1234567-1234567 and 222-1234567-1234567"), "missing_or_ambiguous_order_reference"},
		{strings.Replace(message("Delivered", "111-1234567-1234567"), "@amazon.com", "@amazon.com.evil.invalid", 1), "unsupported_sender"},
		{strings.Replace(message("Delivered", "111-1234567-1234567"), "Tue, 27 Oct 2026 10:00:00 -0400", "not a date", 1), "missing_or_invalid_event_date"},
	}
	for _, tc := range tests {
		out, err := Parse(strings.NewReader(tc.raw))
		if err != nil || out.Status != "exception" || out.Reason != tc.reason || out.Event != nil {
			t.Fatalf("expected %s, got %+v %v", tc.reason, out, err)
		}
	}
	// An order confirmation can be useful without prices or a tracking number.
	out, _ := Parse(strings.NewReader(message("Your Amazon Fresh order", "111-1234567-1234567")))
	if out.Event.TotalMinor != nil || out.Event.TrackingNumber != "" {
		t.Fatal("invented missing fields")
	}
	out, _ = Parse(strings.NewReader(message("Shipped", "111-1234567-1234567\nTracking number: ABC123456\nTracking number: DEF123456")))
	if out.Event.TrackingNumber != "" {
		t.Fatal("guessed ambiguous shipment")
	}
}
func TestMIMEAndLimits(t *testing.T) {
	header := "From: ship-confirm@amazon.com\r\nMessage-ID: <mime@example.invalid>\r\nDate: Tue, 27 Oct 2026 10:00:00 +0000\r\nSubject: Shipped\r\n"
	html := `<html><script>222-1234567-1234567</script><p>Order 111-1234567-1234567</p><p>Tracking number: TBA123456789012</p></html>`
	raw := header + "Content-Type: multipart/mixed; boundary=synthetic\r\n\r\n--synthetic\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(html)) + "\r\n--synthetic\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=x.txt\r\n\r\n222-1234567-1234567\r\n--synthetic--\r\n"
	out, err := Parse(strings.NewReader(raw))
	if err != nil || out.Status != "parsed" || out.Event.OrderReference != "111-1234567-1234567" {
		t.Fatalf("MIME %+v %v", out, err)
	}
	if _, err = Parse(strings.NewReader(strings.Repeat("x", MaxMessageBytes+1))); err == nil {
		t.Fatal("oversized message accepted")
	}
	out, _ = Parse(strings.NewReader(header + "Content-Type: text/plain; charset=unknown\r\n\r\n111-1234567-1234567"))
	if out.Status != "exception" {
		t.Fatal("unsupported charset guessed")
	}
	raw = message("Your Amazon Fresh order", "111-1234567-1234567")
	raw = strings.Replace(raw, "Message-ID: <synthetic-one@example.invalid>\r\n", "", 1)
	a, _ := Parse(strings.NewReader(raw))
	b, _ := Parse(strings.NewReader(raw))
	if a.MessageKey != b.MessageKey || !strings.HasPrefix(a.MessageKey, "sha256:") {
		t.Fatal("missing message ID not stable")
	}
}
