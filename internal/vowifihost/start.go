package vowifihost

import (
	"context"
	"strings"
	"time"

	"github.com/1239t/vohive/pkg/logger"
	"github.com/1239t/vowifi-go/runtimehost"
)

func (m *Manager) BeginStart(deviceID string) StartClaim {
	if m == nil {
		return StartClaim{}
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return StartClaim{}
	}
	return m.RuntimeStore().BeginStart(deviceID)
}

func (m *Manager) FailStart(deviceID string, epoch uint64, state runtimehost.State, err error) {
	if m == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	m.RuntimeStore().FailStart(deviceID, epoch, state, err)
	m.BroadcastState(deviceID)
}

func (m *Manager) CurrentEpoch(deviceID string) uint64 {
	if m == nil {
		return 0
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return 0
	}
	return m.RuntimeStore().CurrentEpoch(deviceID)
}

func (m *Manager) ShouldRun(deviceID string, epoch uint64) bool {
	return m.CurrentEpoch(deviceID) == epoch
}

func (m *Manager) ClaimStarted(deviceID string, epoch uint64, inst *runtimehost.Instance) bool {
	if m == nil || inst == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	current := m.CurrentEpoch(deviceID)
	if current != epoch {
		logger.Info("丢弃过期 VoWiFi 启动结果",
			"device", deviceID,
			"startup_epoch", epoch,
			"current_epoch", current)
		return false
	}
	if old := m.RuntimeStore().Instance(deviceID); old != nil && old != inst {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = old.Stop(stopCtx)
		cancel()
		logger.Info("VoWiFi 新实例接管前已停止旧 pipeline",
			"device", deviceID,
			"startup_epoch", epoch)
	}
	return m.RuntimeStore().ClaimStarted(deviceID, epoch, inst)
}

func (m *Manager) IsCurrentInstance(deviceID string, inst *runtimehost.Instance) bool {
	if m == nil || inst == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	return m.RuntimeStore().Instance(deviceID) == inst
}
