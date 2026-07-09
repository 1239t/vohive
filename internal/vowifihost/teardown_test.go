package vowifihost

import (
	"context"
	"testing"
	"time"

	"github.com/1239t/vowifi-go/runtimehost"
)

func TestManagerStopInstanceForTeardownDeletesAndBroadcasts(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-stop"
	manager.RuntimeStore().SetInstance(deviceID, &runtimehost.Instance{})
	ch, unsub := manager.SubscribeState(deviceID)
	defer unsub()

	if !manager.StopInstanceForTeardown(context.Background(), deviceID, "test") {
		t.Fatal("StopInstanceForTeardown() = false, want true")
	}
	if manager.RuntimeStore().Instance(deviceID) != nil {
		t.Fatal("runtime instance should be deleted")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected teardown broadcast")
	}
}

func TestManagerStopInstanceForTeardownNormalizesDeviceIDBeforeBroadcast(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-trim"
	manager.RuntimeStore().SetInstance(deviceID, &runtimehost.Instance{})
	ch, unsub := manager.SubscribeState(deviceID)
	defer unsub()

	if !manager.StopInstanceForTeardown(context.Background(), " "+deviceID+" ", "test") {
		t.Fatal("StopInstanceForTeardown() = false, want true")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected teardown broadcast on normalized device ID")
	}
}

func TestManagerTeardownSessionInvalidatesAndRunsSMSHook(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-teardown"
	manager.RuntimeStore().SetInstance(deviceID, &runtimehost.Instance{})
	before := manager.RuntimeStore().CurrentEpoch(deviceID)

	var restored string
	if !manager.TeardownSession(context.Background(), deviceID, TeardownOptions{
		Reason:     "switch",
		RestoreSMS: true,
		RestoreSMSMode: func(deviceID string) {
			restored = deviceID
		},
	}) {
		t.Fatal("TeardownSession() = false, want true")
	}
	if got := manager.RuntimeStore().CurrentEpoch(deviceID); got != before+1 {
		t.Fatalf("epoch = %d, want %d", got, before+1)
	}
	if restored != deviceID {
		t.Fatalf("restore hook device = %q, want %q", restored, deviceID)
	}
}
