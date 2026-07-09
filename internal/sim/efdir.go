package sim

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/1239t/vohive/pkg/logger"
)

// ResolveAIDViaEFDIR 通过"开引导通道 + 在通道上发 APDU 读 EF_DIR"解析出 hex 以
// expectedPrefix 开头的完整 AID。传输无关:只依赖 ATModem 的 OpenLogicalChannel/
// TransmitAPDU,因此 QMI/MBIM/AT 都能用。用作各 backend 原生 AID 解析
// (QMI card-status / MBIM APPLICATION_LIST)不可用时的共用兜底。
func ResolveAIDViaEFDIR(m ATModem, bootstrapAID, expectedPrefix string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("efdir: nil modem")
	}
	boot := strings.ToUpper(strings.TrimSpace(bootstrapAID))
	ch, err := m.OpenLogicalChannel(boot)
	if err != nil {
		return "", fmt.Errorf("efdir: 开引导通道失败: %w", err)
	}
	logger.Debug("efdir: 引导通道已开", "bootstrap_aid", boot, "channel", ch)
	defer func() { _ = m.CloseLogicalChannel(ch) }()
	// CLA=0x00:MBIM/QMI 的逻辑通道 APDU 由模组按 transmit 的 channel 句柄路由,APDU
	// 自身 CLA 用基础通道(模组内部补通道位)。
	const cla = 0x00

	// 预热:用 ISD-R(eUICC GlobalPlatform 安全域)AID 开出的通道,初始处于 GP 上下文,
	// 直接 SELECT-by-FID(P1=00,选 MF)会被拒 6D00。必须先发一条 SELECT-by-AID(P1=04)
	// 把通道切出 GP 上下文,之后 SELECT-by-FID 才工作。这里用 USIM 短前缀做预热,结果
	// 忽略(短 AID 大概率 6A82 选不中,但已完成上下文切换)。
	_, _, _, _ = efdirTransmit(m, ch, selectByAIDAPDU(cla, []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x02}))

	if _, s1, s2, err := efdirTransmit(m, ch, selectFileAPDU(cla, 0x3F00)); err != nil || !isAPDUSuccess(s1, s2) {
		return "", fmt.Errorf("efdir: SELECT MF 失败(ch=%d) sw=%02X%02X err=%v", ch, s1, s2, err)
	}
	if _, s1, s2, err := efdirTransmit(m, ch, selectFileAPDU(cla, 0x2F00)); err != nil || !isAPDUSuccess(s1, s2) {
		return "", fmt.Errorf("efdir: SELECT EF_DIR 失败(ch=%d) sw=%02X%02X err=%v", ch, s1, s2, err)
	}

	want := strings.ToUpper(strings.TrimSpace(expectedPrefix))
	const maxRecords = 16
	for rec := 1; rec <= maxRecords; rec++ {
		body, sw1, sw2, err := efdirTransmit(m, ch, readRecordAPDU(cla, byte(rec)))
		if err != nil {
			return "", fmt.Errorf("efdir: 读 EF_DIR 记录 %d 失败: %w", rec, err)
		}
		if isRecordNotFound(sw1, sw2) {
			break
		}
		if aid := extractAID4F(body); len(aid) > 0 {
			aidHex := strings.ToUpper(hex.EncodeToString(aid))
			if strings.HasPrefix(aidHex, want) && len(aidHex) > len(want) {
				return aidHex, nil
			}
		}
	}
	return "", fmt.Errorf("efdir: EF_DIR 未发现匹配前缀 %s 的应用", want)
}

func selectFileAPDU(cla byte, fid uint16) []byte {
	return []byte{cla, 0xA4, 0x00, 0x04, 0x02, byte(fid >> 8), byte(fid)}
}

// selectByAIDAPDU 构造 SELECT-by-AID(P1=04, P2=00 第一个/唯一匹配 + 返回 FCI)。
func selectByAIDAPDU(cla byte, aid []byte) []byte {
	apdu := []byte{cla, 0xA4, 0x04, 0x00, byte(len(aid))}
	return append(apdu, aid...)
}

func readRecordAPDU(cla, rec byte) []byte {
	return []byte{cla, 0xB2, rec, 0x04, 0x00}
}

// isAPDUSuccess 字节序鲁棒地判定成功状态字(9000/91xx/61xx)。
func isAPDUSuccess(sw1, sw2 byte) bool {
	for _, b := range []byte{sw1, sw2} {
		switch b {
		case 0x90, 0x91, 0x61:
			return true
		}
	}
	return false
}

// isRecordNotFound 字节序鲁棒地判定"记录不存在/超出末尾"(6A83/6A82)。
// 这颗模组把 SW 以小端塞进 status 字段,拆出来字节序会反,故两种顺序都认。
func isRecordNotFound(sw1, sw2 byte) bool {
	return (sw1 == 0x6A && (sw2 == 0x83 || sw2 == 0x82)) ||
		(sw2 == 0x6A && (sw1 == 0x83 || sw1 == 0x82))
}

func efdirTransmit(m ATModem, ch int, apdu []byte) (body []byte, sw1, sw2 byte, err error) {
	apduHex := strings.ToUpper(hex.EncodeToString(apdu))
	respHex, err := m.TransmitAPDU(ch, apduHex)
	if err != nil {
		logger.Debug("efdir: APDU 发送失败", "channel", ch, "apdu", apduHex, "err", err)
		return nil, 0, 0, err
	}
	logger.Debug("efdir: APDU 往返", "channel", ch, "apdu", apduHex, "resp", strings.ToUpper(respHex))
	resp, err := hex.DecodeString(respHex)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("APDU 响应 hex 解码失败: %w", err)
	}
	if len(resp) < 2 {
		return resp, 0, 0, nil
	}
	return resp[:len(resp)-2], resp[len(resp)-2], resp[len(resp)-1], nil
}

// extractAID4F 从 EF_DIR 记录里取 tag 4F(Application ID)的值,支持嵌在
// 61(Application template)模板内。
func extractAID4F(rec []byte) []byte {
	for i := 0; i+1 < len(rec); {
		tag := rec[i]
		ln := int(rec[i+1])
		if i+2+ln > len(rec) {
			break
		}
		val := rec[i+2 : i+2+ln]
		switch tag {
		case 0x4F:
			return val
		case 0x61:
			if aid := extractAID4F(val); len(aid) > 0 {
				return aid
			}
		}
		i += 2 + ln
	}
	return nil
}
