package device

import (
	"errors"
	"testing"

	"github.com/1239t/vowifi-go/runtimehost/carrier"
)

func TestShouldRetryVoWiFiAutoStart(t *testing.T) {
	if shouldRetryVoWiFiAutoStart(nil) {
		t.Fatalf("nil error should not enter retry path")
	}
	if !shouldRetryVoWiFiAutoStart(errors.New("temporary failure")) {
		t.Fatalf("non-policy error should keep retry behavior")
	}
	if shouldRetryVoWiFiAutoStart(carrier.NewVoWiFiBlockedMCCError("460")) {
		t.Fatalf("policy-blocked error should not retry")
	}
}
