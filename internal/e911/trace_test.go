package e911

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func TestFormatEntitlementTraceBodyRedactsSensitiveFields(t *testing.T) {
	body := []byte(`[
		{
			"token":"token-value",
			"app-token":"app-token-value",
			"auth-type":"EAP-AKA",
			"action-name":"getAuthentication",
			"subscriber-id":"subscriber-value",
			"unique-id":"356306952769025",
			"request-id":4
		},
		{
			"payload":"challenge-response-value",
			"action-name":"postChallenge",
			"request-id":8
		},
		{
			"status":6000,
			"response-id":5,
			"address-update-url-post-data":"method%3Dupdate-tc-loc%26authtoken%3Dsecret-token"
		}
	]`)

	got := formatEntitlementTraceBody(body, false)
	for _, secret := range []string{
		"token-value",
		"app-token-value",
		"subscriber-value",
		"356306952769025",
		"challenge-response-value",
		"secret-token",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("trace body leaked %q: %s", secret, got)
		}
	}
	for _, marker := range []string{
		`"action-name":"getAuthentication"`,
		`"action-name":"postChallenge"`,
		`"status":6000`,
		`"[REDACTED`,
		`sha256=`,
	} {
		if !strings.Contains(got, marker) {
			t.Fatalf("trace body missing %q: %s", marker, got)
		}
	}
}

func TestFormatEntitlementTraceBodyIncludesStableSensitiveFingerprints(t *testing.T) {
	body := []byte(`{"unique-id":"356306952769025","action-name":"getAuthentication"}`)

	got := formatEntitlementTraceBody(body, false)
	if strings.Contains(got, "356306952769025") {
		t.Fatalf("trace body leaked unique-id: %s", got)
	}
	if !strings.Contains(got, `"unique-id":"[REDACTED len=15 sha256=54f1b6d5c4a4]"`) {
		t.Fatalf("trace body missing stable unique-id fingerprint: %s", got)
	}
}

func TestFormatEntitlementTraceBodyAnnotatesSubscriberIDNAI(t *testing.T) {
	body := []byte(`{"subscriber-id":"AgAAOwEwMzEwMjgwMjMzNjg4NDk0QG5haS5lcGMubW5jMjgwLm1jYzMxMC4zZ3BwbmV0d29yay5vcmc=","action-name":"getAuthentication"}`)

	got := formatEntitlementTraceBody(body, false)
	if strings.Contains(got, "310990000000002") {
		t.Fatalf("trace body leaked IMSI: %s", got)
	}
	for _, marker := range []string{
		`nai_mcc=310`,
		`nai_mnc=280`,
		`imsi_sha256=6ff7323c9fa9`,
	} {
		if !strings.Contains(got, marker) {
			t.Fatalf("trace body missing %q: %s", marker, got)
		}
	}
}

func TestFormatEntitlementTraceBodyRawOptInKeepsBody(t *testing.T) {
	body := []byte(`{"token":"token-value","action-name":"getAuthentication"}`)

	got := formatEntitlementTraceBody(body, true)
	if !strings.Contains(got, `"token":"token-value"`) {
		t.Fatalf("raw trace body should keep original payload: %s", got)
	}
}

func TestDecodeEntitlementTraceBodyHandlesGzip(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte(`{"action-name":"getAuthentication"}`)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	got := decodeEntitlementTraceBody(compressed.Bytes())
	if string(got) != `{"action-name":"getAuthentication"}` {
		t.Fatalf("decoded body=%q", got)
	}
}

func TestExtractEntitlementPhoneNumberResponses(t *testing.T) {
	got := extractEntitlementPhoneNumberResponses([]byte(`[
		{"status":6000,"response-id":7,"phone-number":"+15035550123","signature":"signed"},
		{"status":6000,"response-id":8}
	]`))
	if len(got) != 1 {
		t.Fatalf("responses=%d", len(got))
	}
	if got[0].PhoneNumber != "+15035550123" || got[0].Signature != "signed" {
		t.Fatalf("response=%+v", got[0])
	}
	masked := redactEntitlementTraceString(got[0].PhoneNumber)
	if masked == got[0].PhoneNumber {
		t.Fatalf("phone number was not masked: %s", masked)
	}
	if !strings.Contains(masked, "+150") || !strings.Contains(masked, "23") {
		t.Fatalf("masked phone number not useful: %s", masked)
	}
	if traceValueFingerprint(got[0].PhoneNumber) == "" {
		t.Fatal("phone number fingerprint empty")
	}
}
