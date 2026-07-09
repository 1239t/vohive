package vowifihost

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	innersim "github.com/1239t/vohive/internal/sim"
	swusim "github.com/1239t/vowifi-go/engine/sim"
	"github.com/1239t/vowifi-go/runtimehost"
	"github.com/1239t/vowifi-go/runtimehost/eventhost"
	"github.com/1239t/vowifi-go/runtimehost/messaging"
	"github.com/1239t/vowifi-go/runtimehost/voicehost"
)

type runtimeStartFunc func(context.Context, runtimehost.StartRequest) (*runtimehost.Instance, error)

type missingSIMProvider struct{}

func (m missingSIMProvider) GetIMSI() (string, error) {
	return "", fmt.Errorf("missing SIM provider")
}
func (m missingSIMProvider) CalculateAKA(rand, autn []byte) (swusim.AKAResult, error) {
	return swusim.AKAResult{}, fmt.Errorf("missing SIM provider")
}
func (m missingSIMProvider) Close() error { return nil }

type modemSIMAdapter struct {
	modem      runtimehost.Modem
	cachedIMSI string
}

func (a *modemSIMAdapter) GetIMSI() (string, error) {
	if strings.TrimSpace(a.cachedIMSI) != "" {
		return a.cachedIMSI, nil
	}
	if a == nil || a.modem == nil {
		return "", fmt.Errorf("read IMSI from modem: modem unavailable")
	}
	id, err := a.modem.GetISIMIdentity()
	if err != nil {
		return "", fmt.Errorf("read IMSI from modem: %w", err)
	}
	if strings.TrimSpace(id.IMSI) == "" {
		return "", fmt.Errorf("read IMSI from modem: IMSI unavailable")
	}
	a.cachedIMSI = strings.TrimSpace(id.IMSI)
	return a.cachedIMSI, nil
}

func (a *modemSIMAdapter) CalculateAKA(randBytes, autnBytes []byte) (swusim.AKAResult, error) {
	if a == nil || a.modem == nil {
		return swusim.AKAResult{}, fmt.Errorf("build AKA APDU: modem unavailable")
	}

	apdu, err := innersim.BuildUSIMAuthAPDU(randBytes, autnBytes, true)
	if err != nil {
		return swusim.AKAResult{}, fmt.Errorf("build AKA APDU: %w", err)
	}

	usimAID := "A0000000871002FF44FF128900000100"
	channel, err := a.modem.OpenLogicalChannel(usimAID)
	if err != nil {
		isimAID := "A0000000871004FFFFFFFF89000000"
		channel, err = a.modem.OpenLogicalChannel(isimAID)
		if err != nil {
			return swusim.AKAResult{}, fmt.Errorf("open USIM/ISIM logical channel: %w", err)
		}
	}
	defer a.modem.CloseLogicalChannel(channel)

	respHex, err := a.modem.TransmitAPDU(channel, hex.EncodeToString(apdu))
	if err != nil {
		return swusim.AKAResult{}, fmt.Errorf("transmit AKA APDU: %w", err)
	}

	respBytes, err := hex.DecodeString(respHex)
	if err != nil {
		return swusim.AKAResult{}, fmt.Errorf("decode AKA response: %w", err)
	}
	return innersim.ParseUSIMAuthResponse("modem", respBytes)
}

func (a *modemSIMAdapter) Close() error { return nil }

// buildVoWiFiSIMAdapter prefers an injected SIM adapter (e.g. MBIM Auth AKA for
// modems without SIM logical-channel APDU); otherwise derives one from the
// modem's APDU path (AT/QMI).
func buildVoWiFiSIMAdapter(override runtimehost.SIMAdapter, modem runtimehost.Modem, imsi string) runtimehost.SIMAdapter {
	imsi = strings.TrimSpace(imsi)
	if override != nil {
		if imsi != "" {
			if liveIMSI, err := override.GetIMSI(); err != nil || strings.TrimSpace(liveIMSI) == "" {
				if p, ok := override.(swusim.AKAProvider); ok {
					return runtimehost.NewReaderSIMAdapterWithIMSI(p, imsi)
				}
			}
		}
		return override
	}
	if modem != nil {
		return &modemSIMAdapter{modem: modem, cachedIMSI: imsi}
	}
	return runtimehost.NewReaderSIMAdapter(missingSIMProvider{})
}

type RuntimeStartRequest struct {
	DeviceID      string
	TraceID       string
	Epoch         uint64
	Prepared      PreparedStart
	Modem         runtimehost.Modem
	Dataplane     runtimehost.DataplanePolicy
	VoiceGateway  *voicehost.Gateway
	DeliveryStore messaging.DeliveryStore
	Dispatch      eventhost.Dispatcher
	BeforeStart   func(context.Context, runtimehost.SessionConfig) error
}

type RuntimeStartResult struct {
	Instance *runtimehost.Instance
	Stale    bool
}

func (m *Manager) SetRuntimeStartForTest(fn runtimeStartFunc) {
	if m == nil {
		return
	}
	m.runtimeStart = fn
}

func (m *Manager) runtimeStarter() runtimeStartFunc {
	if m != nil && m.runtimeStart != nil {
		return m.runtimeStart
	}
	return runtimehost.Start
}

func (m *Manager) StartRuntime(ctx context.Context, req RuntimeStartRequest) (RuntimeStartResult, error) {
	if m == nil {
		return RuntimeStartResult{}, fmt.Errorf("vowifi host manager is nil")
	}
	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		return RuntimeStartResult{}, fmt.Errorf("vowifi runtime start device_id is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	traceID := strings.TrimSpace(req.TraceID)
	if traceID == "" {
		traceID = runtimehost.NewTraceID()
	}

	prepared := req.Prepared.Prepared
	profile := prepared.Profile
	if strings.TrimSpace(profile.IMSI) == "" {
		profile = req.Prepared.Profile
	}
	networkMode := strings.TrimSpace(req.Prepared.StartupState.NetworkMode)
	if networkMode == "" {
		networkMode = strings.TrimSpace(req.Prepared.NetworkMode)
	}

	inst, err := m.runtimeStarter()(ctx, runtimehost.StartRequest{
		Mode:          runtimehost.StartModeMain,
		DeviceID:      deviceID,
		TraceID:       traceID,
		Profile:       profile,
		Prepared:      &prepared,
		NetworkMode:   networkMode,
		VoiceGateway:  req.VoiceGateway,
		SIM:           buildVoWiFiSIMAdapter(req.Prepared.SIM, req.Modem, prepared.Profile.IMSI),
		Access:        runtimehost.NewModemAccessAdapter(req.Modem),
		Dataplane:     req.Dataplane,
		Proxy:         req.Prepared.Proxy,
		PCSCFAddr:       req.Prepared.PCSCFAddr,
		CellID:          req.Prepared.CellID,
		RegisterProfile: req.Prepared.RegisterProfile,
		SIPInstanceURN:  req.Prepared.SIPInstanceURN,
		RegisterExpiry:  req.Prepared.RegisterExpiry,
		DeliveryStore:   req.DeliveryStore,
		Dispatch:      req.Dispatch,
		BeforeStart:   req.BeforeStart,
		ShouldRun: func() bool {
			return ctx.Err() == nil && m.ShouldRun(deviceID, req.Epoch)
		},
	})
	if err != nil {
		return RuntimeStartResult{}, err
	}

	inst.AddObserver(runtimehost.ObserverFunc(func(_ context.Context, ev runtimehost.Event) {
		if m.IsCurrentInstance(deviceID, inst) {
			m.BroadcastState(deviceID)
			return
		}
		m.RecordStartupState(deviceID, ev.State)
	}))

	if !m.ClaimStarted(deviceID, req.Epoch, inst) {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = inst.Stop(stopCtx)
		cancel()
		m.ClearStartupStateAndBroadcast(deviceID)
		return RuntimeStartResult{Instance: inst, Stale: true}, nil
	}

	return RuntimeStartResult{Instance: inst}, nil
}