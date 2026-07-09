package vowifihost

import (
	"errors"
	"testing"
	"time"

	"github.com/1239t/vowifi-go/runtimehost"
)

func TestManagerBeginAndFailStartOwnsStartupMutationAndBroadcast(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-start"
	ch, unsub := manager.SubscribeState(deviceID)
	defer unsub()

	claim := manager.BeginStart(deviceID)
	if !claim.Accepted {
		t.Fatalf("BeginStart() = %+v, want Accepted", claim)
	}
	if !manager.RuntimeStore().Starting(deviceID) {
		t.Fatal("runtime should be marked starting")
	}

	manager.FailStart(deviceID, claim.Epoch, runtimehost.State{DeviceID: deviceID}, errors.New("start failed"))

	if manager.RuntimeStore().Starting(deviceID) {
		t.Fatal("runtime starting flag should be cleared")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected failure broadcast")
	}
}

func TestManagerShouldRunMatchesRuntimeEpoch(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-epoch"
	claim := manager.BeginStart(deviceID)

	if !manager.ShouldRun(deviceID, claim.Epoch) {
		t.Fatal("ShouldRun() = false for current epoch, want true")
	}
	manager.InvalidateRuntime(deviceID, "test")
	if manager.ShouldRun(deviceID, claim.Epoch) {
		t.Fatal("ShouldRun() = true for stale epoch, want false")
	}
}

func TestManagerClaimStartedAcceptsCurrentAndRejectsStaleEpoch(t *testing.T) {
	manager := NewManager()
	deviceID := "dev-claim"
	stale := manager.CurrentEpoch(deviceID)
	manager.InvalidateRuntime(deviceID, "test")

	if manager.ClaimStarted(deviceID, stale, &runtimehost.Instance{}) {
		t.Fatal("ClaimStarted() = true for stale epoch, want false")
	}
	if manager.RuntimeStore().Active(deviceID) {
		t.Fatal("stale claim should not activate runtime")
	}

	current := manager.CurrentEpoch(deviceID)
	inst := &runtimehost.Instance{}
	if !manager.ClaimStarted(deviceID, current, inst) {
		t.Fatal("ClaimStarted() = false for current epoch, want true")
	}
	if manager.RuntimeStore().Instance(deviceID) != inst {
		t.Fatal("current claim should store active instance")
	}
}
