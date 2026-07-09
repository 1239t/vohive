package vowifihost

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/1239t/vowifi-go/runtimehost"
	"github.com/1239t/vowifi-go/runtimehost/identity"
	"github.com/1239t/vowifi-go/runtimehost/voiceclient"
)

type PreparedStart struct {
	Profile      identity.Profile
	Prepared     identity.PreparedSession
	Modem        runtimehost.Modem
	SIM          runtimehost.SIMAdapter // optional override; when nil, derived from Modem APDU
	Proxy        *runtimehost.ProxyConfig
	PCSCFAddr        string
	CellID           string
	RegisterProfile  voiceclient.RegisterProfile
	SIPInstanceURN   string
	RegisterExpiry   time.Duration
	NetworkMode      string
	StartupState runtimehost.State
}

type Adapter interface {
	Context() context.Context
	IsSwitching(deviceID string) bool
	WorkerExists(deviceID string) bool
	WaitQMICoreReady(deviceID string, timeout time.Duration) error
	WaitWorkerReady(deviceID string, timeout time.Duration) error
	PrepareStart(deviceID, traceID, runtimeEPDGOverride string) (PreparedStart, error)
	BeforeStart(deviceID string, modem runtimehost.Modem, proxy *runtimehost.ProxyConfig) func(context.Context, runtimehost.SessionConfig) error
	HandleStartupError(req StartupErrorRequest) error
	MarkRuntimeStarted(req RuntimeStartedRequest)
	RestoreSMSMode(deviceID string)
	RestoreRadioAfterVoWiFi(deviceID string) error
}

type StartupErrorRequest struct {
	TraceID             string
	DeviceID            string
	RuntimeEPDGOverride string
	Generation          uint64
	StartedAt           time.Time
	State               runtimehost.State
	Err                 error
}

type RuntimeStartedRequest struct {
	TraceID     string
	DeviceID    string
	ActiveCount int
	Elapsed     time.Duration
}

func (m *Manager) ConfigureAdapter(adapter Adapter) {
	if m == nil {
		return
	}
	m.adapter = adapter
}

func (m *Manager) PrepareStart(deviceID, traceID, runtimeEPDGOverride string) (PreparedStart, error) {
	adapter := m.hostAdapter()
	if adapter == nil {
		return PreparedStart{}, fmt.Errorf("vowifi host adapter is not configured")
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return PreparedStart{}, fmt.Errorf("vowifi prepare start device_id is empty")
	}
	return adapter.PrepareStart(deviceID, strings.TrimSpace(traceID), strings.TrimSpace(runtimeEPDGOverride))
}

func (m *Manager) BeforeStart(deviceID string, modem runtimehost.Modem, proxy *runtimehost.ProxyConfig) func(context.Context, runtimehost.SessionConfig) error {
	adapter := m.hostAdapter()
	if adapter == nil {
		return nil
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil
	}
	return adapter.BeforeStart(deviceID, modem, proxy)
}

func (m *Manager) hostAdapter() Adapter {
	if m == nil {
		return nil
	}
	return m.adapter
}
