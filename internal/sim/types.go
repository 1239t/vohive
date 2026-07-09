package sim

import (
	"errors"

	swusim "github.com/1239t/vowifi-go/engine/sim"
)

const (
	AKAAppPreferenceUSIM       = "usim"        // 优先使用 USIM 卡进行认证
	AKAAppPreferenceAuto       = "auto"        // 自动选择（ISIM 优先，回退 USIM）
	AKAAppPreferenceISIM       = "isim"        // 优先使用 ISIM 卡进行认证
	AKAAppPreferenceISIMStrict = "isim_strict" // 强制且仅能使用 ISIM 卡进行认证
)

// ErrAPDUBusy 指示 APDU 通道处于繁忙状态，无法响应本次 AKA 计算请求
var ErrAPDUBusy = errors.New("apdu busy")

// AKAResult 复用 swu-go 的统一 AKA 结果类型，避免在公共边界重复定义。
type AKAResult = swusim.AKAResult

// AKAProvider 复用 swu-go 的统一 AKAProvider 契约。
type AKAProvider = swusim.AKAProvider
