package vowifihost

import (
	"context"
	"testing"
	"time"

	"github.com/1239t/vowifi-go/runtimehost"
)

func TestManagerDesiredRecoverableIsFalseWhenRuntimeActiveOrStarting(t *testing.T) {
	manager := NewManager()
	manager.RuntimeStore().SetInstance("active", &runtimehost.Instance{})
	manager.RuntimeStore().BeginStart("starting")

	if manager.DesiredRecoverable("active") {
		t.Fatal("DesiredRecoverable() = true for active runtime, want false")
	}
	if manager.DesiredRecoverable("starting") {
		t.Fatal("DesiredRecoverable() = true for starting runtime, want false")
	}
	if !manager.DesiredRecoverable("idle") {
		t.Fatal("DesiredRecoverable() = false for idle device, want true")
	}
}

func TestManagerScheduleDesiredRecoverRunsRecoverAndCallback(t *testing.T) {
	manager := NewManager()
	commands := make(chan LifecycleCommand, 1)
	callbacks := make(chan error, 1)
	manager.SetLifecycleRunForTest(func(ctx context.Context, cmd LifecycleCommand) error {
		commands <- cmd
		return nil
	})

	if !manager.ScheduleDesiredRecover(context.Background(), DesiredRecoverRequest{
		DeviceID: "dev-1",
		Reason:   "desired_reconcile",
		Now:      time.Now(),
		OnResult: func(deviceID, reason string, err error) {
			if deviceID != "dev-1" {
				t.Errorf("callback deviceID = %q, want dev-1", deviceID)
			}
			if reason != "desired_reconcile" {
				t.Errorf("callback reason = %q, want desired_reconcile", reason)
			}
			callbacks <- err
		},
	}) {
		t.Fatal("ScheduleDesiredRecover() = false, want true")
	}

	select {
	case cmd := <-commands:
		if cmd.Kind != LifecycleCommandRecover {
			t.Fatalf("command kind = %s, want recover", cmd.Kind.String())
		}
		if cmd.DeviceID != "dev-1" {
			t.Fatalf("command deviceID = %q, want dev-1", cmd.DeviceID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recover command")
	}

	select {
	case err := <-callbacks:
		if err != nil {
			t.Fatalf("callback err = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recover callback")
	}
}

func TestManagerScheduleDesiredRecoverSkipsRuntimeActivity(t *testing.T) {
	manager := NewManager()
	manager.RuntimeStore().SetInstance("dev-1", &runtimehost.Instance{})
	manager.SetLifecycleRunForTest(func(ctx context.Context, cmd LifecycleCommand) error {
		t.Fatalf("recover should not run for active runtime: %+v", cmd)
		return nil
	})

	if manager.ScheduleDesiredRecover(context.Background(), DesiredRecoverRequest{
		DeviceID: "dev-1",
		Now:      time.Now(),
		OnResult: func(deviceID, reason string, err error) {
			t.Fatalf("callback should not run for skipped recover")
		},
	}) {
		t.Fatal("ScheduleDesiredRecover() = true for active runtime, want false")
	}
}
