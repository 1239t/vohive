package device

import (
	"fmt"
	"strings"

	"github.com/1239t/vohive/internal/backend"
	"github.com/1239t/vohive/internal/modem"
)

func newWorkerBackendStrict(deviceID, backendMode, controlDevice string, m *modem.Manager, source backend.QMISource, mbimSource backend.MBIMSource) (backend.DeviceBackend, error) {
	be, err := backend.NewBackend(backendMode, controlDevice, m, source, mbimSource)
	if err != nil {
		prefix := ""
		if id := strings.TrimSpace(deviceID); id != "" {
			prefix = fmt.Sprintf("[%s] ", id)
		}
		return nil, fmt.Errorf("%s初始化 %s 后端失败: %w", prefix, backendMode, err)
	}
	return be, nil
}

func backendUsesATRuntime(mode string) bool {
	return backend.NormalizeBackendMode(mode) == backend.BackendAT
}

func workerUsesATRuntime(w *Worker) bool {
	if w == nil {
		return false
	}
	if w.Backend != nil {
		return w.Backend.Mode() == backend.BackendAT
	}
	return backendUsesATRuntime(resolvedBackendMode(w.Config))
}

func workerBackendMode(w *Worker) string {
	if w == nil {
		return ""
	}
	if w.Backend != nil {
		return w.Backend.Mode()
	}
	return resolvedBackendMode(w.Config)
}
