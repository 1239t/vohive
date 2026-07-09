package device

import (
	"fmt"

	"github.com/1239t/vohive/internal/backend"
	"github.com/1239t/vohive/internal/cscall"
	"github.com/1239t/vohive/internal/sipgw"
	"github.com/1239t/vohive/pkg/logger"
)

func newCSCallManagerForWorker(w *Worker, r *sipgw.Registrar) *cscall.Manager {
	if w == nil || r == nil || w.Config.AudioDevice == "" {
		return nil
	}
	switch {
	case w.Backend != nil && w.Backend.Mode() == backend.BackendAT && w.Modem != nil:
		return cscall.NewManagerWithController(w.ID, w.Config.AudioDevice, cscall.NewATController(w.Modem), r)
	case w.Backend != nil && w.Backend.Mode() == backend.BackendQMI && w.QMICore != nil:
		return cscall.NewManagerWithController(w.ID, w.Config.AudioDevice, cscall.NewQMIController(w.QMICore), r)
	default:
		logger.Debug(fmt.Sprintf("[%s] 跳过 CS 域语音桥接：缺少可用控制面", w.ID),
			"backend", workerBackendMode(w),
			"audio_device", w.Config.AudioDevice,
			"has_modem", w.Modem != nil,
			"has_qmi_core", w.QMICore != nil)
		return nil
	}
}
