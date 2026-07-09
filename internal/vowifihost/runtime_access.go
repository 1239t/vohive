package vowifihost

import (
	"strings"

	"github.com/1239t/vowifi-go/runtimehost"
)

func (m *Manager) Instance(deviceID string) *runtimehost.Instance {
	if m == nil {
		return nil
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil
	}
	return m.RuntimeStore().Instance(deviceID)
}

func (m *Manager) Instances() map[string]*runtimehost.Instance {
	if m == nil {
		return nil
	}
	return m.RuntimeStore().Instances()
}

func (m *Manager) InstanceIDs() []string {
	if m == nil {
		return nil
	}
	return m.RuntimeStore().InstanceIDs()
}

func (m *Manager) Active(deviceID string) bool {
	if m == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	return m.RuntimeStore().Active(deviceID)
}

func (m *Manager) Starting(deviceID string) bool {
	if m == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	return m.RuntimeStore().Starting(deviceID)
}

func (m *Manager) State(deviceID string) (runtimehost.State, bool) {
	if m == nil {
		return runtimehost.State{}, false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return runtimehost.State{}, false
	}
	return m.RuntimeStore().State(deviceID)
}
