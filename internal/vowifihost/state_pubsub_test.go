package vowifihost

import (
	"testing"
	"time"

	"github.com/1239t/vowifi-go/runtimehost"
)

func TestManagerStateSubscriptionBroadcastAndCleanup(t *testing.T) {
	manager := NewManager()

	ch, unsub := manager.SubscribeState("dev-1")
	if got := manager.SubscriberCount("dev-1"); got != 1 {
		t.Fatalf("SubscriberCount() = %d, want 1", got)
	}

	manager.BroadcastState("dev-1")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected state broadcast")
	}

	unsub()
	if got := manager.SubscriberCount("dev-1"); got != 0 {
		t.Fatalf("SubscriberCount() after unsubscribe = %d, want 0", got)
	}

	manager.BroadcastState("dev-1")
	select {
	case <-ch:
		t.Fatal("unexpected broadcast after unsubscribe")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestManagerRecordStartupStateBroadcastsWhenAccepted(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-starting"
	ch, unsub := manager.SubscribeState(deviceID)
	defer unsub()

	accepted := manager.RecordStartupState(deviceID, runtimehost.State{
		DeviceID:   deviceID,
		Phase:      "radio_ready",
		UpdatedAt:  time.Now(),
		LastReason: "starting",
	})
	if !accepted {
		t.Fatal("expected startup state to be accepted")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected broadcast for accepted startup state")
	}

	rejected := manager.RecordStartupState(deviceID, runtimehost.State{
		DeviceID:  deviceID,
		Phase:     "older",
		UpdatedAt: time.Now().Add(-time.Hour),
	})
	if rejected {
		t.Fatal("expected stale startup state to be rejected")
	}
	select {
	case <-ch:
		t.Fatal("unexpected broadcast for rejected startup state")
	case <-time.After(50 * time.Millisecond):
	}
}
