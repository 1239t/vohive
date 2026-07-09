package device

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/1239t/vohive/internal/backend"
	"github.com/1239t/vowifi-go/runtimehost/carrier"
	"github.com/1239t/vowifi-go/runtimehost/voiceclient"
)

func waitVoWiFiServingCellID(ctx context.Context, w *Worker, timeout time.Duration) {
	if w == nil || timeout <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_ = waitForCondition(waitCtx, 500*time.Millisecond, func() bool {
		return resolveVoWiFiIMSUTRANCellIDFromQMI(waitCtx, w) != ""
	})
}

func resolveVoWiFiIMSUTRANCellID(ctx context.Context, w *Worker, mcc, mnc string) (suffix, source string) {
	switch carrier.IMSCellIDMode(mcc, mnc) {
	case "carrier_only":
		if suffix = carrier.DefaultUTRANCellIDSuffix(mcc, mnc); suffix != "" {
			return suffix, "carrier_default"
		}
		return "", ""
	case "none":
		return "", "none"
	}
	if suffix = resolveVoWiFiIMSUTRANCellIDFromQMI(ctx, w); suffix != "" {
		return suffix, "qmi"
	}
	if suffix = carrier.DefaultUTRANCellIDSuffix(mcc, mnc); suffix != "" {
		return suffix, "carrier_default"
	}
	return "", ""
}

func resolveVoWiFiIMSUTRANCellIDFromQMI(ctx context.Context, w *Worker) string {
	if w == nil {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_ = w.RefreshRuntime(ctx, "vowifi_cell_id")

	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if suffix := utranCellIDSuffixFromQMICore(queryCtx, w); suffix != "" {
		return suffix
	}

	if w.Backend != nil {
		if ss, err := w.Backend.GetServingSystem(queryCtx); err == nil && ss != nil {
			if suffix := utranCellIDSuffixFromServingSystem(ss); suffix != "" {
				return suffix
			}
		}
	}

	w.cacheMu.RLock()
	lac := strings.TrimSpace(w.state.Runtime.LAC)
	cellID := strings.TrimSpace(w.state.Runtime.CellID)
	w.cacheMu.RUnlock()
	return utranCellIDSuffixFromHex(lac, cellID)
}

func utranCellIDSuffixFromQMICore(ctx context.Context, w *Worker) string {
	if w == nil || w.QMICore == nil {
		return ""
	}
	if cellInfo, err := w.QMICore.NASGetCellLocationInfo(ctx); err == nil && cellInfo != nil {
		if cellInfo.LTE != nil && cellInfo.LTE.TAC > 0 && cellInfo.LTE.GlobalCellID > 0 {
			return voiceclient.FormatUTRANCellIDSuffix(uint32(cellInfo.LTE.TAC), cellInfo.LTE.GlobalCellID)
		}
		if cellInfo.NR5G != nil && cellInfo.NR5G.TAC > 0 && cellInfo.NR5G.GlobalCellID > 0 {
			return voiceclient.FormatUTRANCellIDSuffix(uint32(cellInfo.NR5G.TAC), uint32(cellInfo.NR5G.GlobalCellID&0x0FFFFFFF))
		}
	}
	if sysInfo, err := w.QMICore.GetSysInfo(ctx); err == nil && sysInfo != nil {
		var tac, eci uint32
		if sysInfo.TAC > 0 {
			tac = uint32(sysInfo.TAC)
		} else if sysInfo.LAC > 0 {
			tac = uint32(sysInfo.LAC)
		}
		if sysInfo.CellID > 0 {
			eci = uint32(sysInfo.CellID & 0x0FFFFFFF)
		}
		if suffix := voiceclient.FormatUTRANCellIDSuffix(tac, eci); suffix != "" {
			return suffix
		}
	}
	return ""
}

func utranCellIDSuffixFromServingSystem(ss *backend.ServingSystem) string {
	if ss == nil {
		return ""
	}
	return utranCellIDSuffixFromHex(strings.TrimSpace(ss.LAC), strings.TrimSpace(ss.CellID))
}

func utranCellIDSuffixFromHex(tacHex, eciHex string) string {
	tac, tacOK := parseHexUint32(tacHex)
	eci, eciOK := parseHexUint32(eciHex)
	if !tacOK && !eciOK {
		return ""
	}
	return voiceclient.FormatUTRANCellIDSuffix(tac, eci)
}

func parseHexUint32(value string) (uint32, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(parsed), true
}