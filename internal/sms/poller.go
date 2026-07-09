package sms

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/1239t/vohive/internal/device"
	"github.com/1239t/vohive/internal/modem"
	"github.com/1239t/vohive/internal/smsnotify"
	"github.com/1239t/vohive/pkg/logger"
	"github.com/1239t/vohive/pkg/smscodec"
)

type Poller struct {
	deviceID      string
	modem         *modem.Manager
	notifier      device.Notifier
	fragmentCache map[string][]*Fragment
	cacheMu       sync.Mutex
}

type Fragment struct {
	Ref     int       // 引用号
	Total   int       // 总分片数
	Seq     int       // 当前序号 (1-based)
	Content string    // 解码后的内容
	Time    time.Time // 接收时间
}

func New(deviceID string, m *modem.Manager, notifier device.Notifier) *Poller {
	return &Poller{
		deviceID:      deviceID,
		modem:         m,
		notifier:      notifier,
		fragmentCache: make(map[string][]*Fragment),
	}
}

func (s *Poller) Start() {
	go func() {
		// 初始化为 PDU 模式
		s.initPDUMode()

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			s.checkSMS()
		}
	}()

	// 启动垃圾回收协程，每分钟清理一次过期分片
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.cleanupOldFragments()
		}
	}()
}

// initPDUMode 初始化模组为 PDU 模式
func (s *Poller) initPDUMode() {
	for i := 0; i < 3; i++ {
		// 1. 关闭回显
		s.modem.ExecuteAT("ATE0", 2*time.Second)

		// 2. 设置 PDU 模式
		_, err := s.modem.ExecuteAT("AT+CMGF=0", 2*time.Second)
		if err == nil {
			logger.Info("短信服务已初始化为 PDU 模式", "device", s.deviceID)
			return
		}
		time.Sleep(1 * time.Second)
	}
	logger.Warn("初始化 PDU 模式失败", "device", s.deviceID)
}

// checkSMS 检查并处理短信 (PDU 模式)
func (s *Poller) checkSMS() {
	// AT+CMGL=4 在 PDU 模式下列出所有短信 (4 = ALL)
	resp, err := s.modem.ExecuteAT("AT+CMGL=4", 10*time.Second)
	if err != nil {
		logger.Warn("检查短信失败", "device", s.deviceID, "err", err)
		return
	}

	// 响应格式:
	// +CMGL: <index>,<status>,,<length>
	// <PDU hex string>
	// OK

	if strings.TrimSpace(resp) == "OK" || !strings.Contains(resp, "+CMGL:") {
		return // 无短信
	}

	lines := strings.Split(resp, "\r\n")
	var processedIndices []int

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || line == "OK" {
			continue
		}

		if strings.HasPrefix(line, "+CMGL:") {
			// 解析索引
			parts := strings.Split(line, ",")
			if len(parts) > 0 {
				idxStr := strings.TrimSpace(strings.TrimPrefix(parts[0], "+CMGL:"))
				if idx, err := strconv.Atoi(idxStr); err == nil {
					processedIndices = append(processedIndices, idx)
				}
			}

			// 下一行是 PDU 数据
			if i+1 < len(lines) {
				pduHex := strings.TrimSpace(lines[i+1])
				if pduHex != "" && pduHex != "OK" {
					if trimmed, ok := smscodec.TrimFullPDUHexByATHeader(pduHex, line); ok {
						pduHex = trimmed
					}
					s.processPDU(pduHex)
				}
				i++ // 跳过 PDU 行
			}
		}
	}

	// 处理完毕后清理短信
	if len(processedIndices) > 0 {
		s.deleteAllMessages()
	}
}

// processPDU 解析 PDU 数据并转发
func (s *Poller) processPDU(raw string) {
	// 十六进制解码
	b, err := hex.DecodeString(raw)
	if err != nil {
		logger.Error("PDU 十六进制解码失败", "device", s.deviceID, "err", err)
		return
	}

	// 处理 SMSC（短信中心地址）头部
	// PDU 第一个字节表示短信中心长度
	if len(b) > 0 {
		smscLen := int(b[0])
		if len(b) > smscLen+1 {
			b = b[smscLen+1:] // 跳过 SMSC 字段
		}
	}

	sender, content, msgTime, concat, err := smscodec.DecodeDeliverTPDU(b)
	if err != nil {
		logger.Error("TPDU 解析失败", "device", s.deviceID, "err", err, "raw", raw)
		return
	}

	timestamp := time.Now()
	if !msgTime.IsZero() {
		timestamp = msgTime
	}

	// 检查是否为长短信分片
	if concat.IsConcat {
		logger.Debug("收到短信分片", "device", s.deviceID, "ref", concat.Ref, "seq", concat.Seq, "total", concat.Total)

		s.cacheMu.Lock()
		key := fmt.Sprintf("%s_%d", sender, concat.Ref)

		// 如果是新的引用号，或者缓存不存在，初始化
		if _, exists := s.fragmentCache[key]; !exists {
			s.fragmentCache[key] = make([]*Fragment, 0)
		}

		// 检查是否已存在该分片（去重）
		exists := false
		for _, f := range s.fragmentCache[key] {
			if f.Seq == concat.Seq {
				exists = true
				break
			}
		}

		if !exists {
			s.fragmentCache[key] = append(s.fragmentCache[key], &Fragment{
				Ref:     concat.Ref,
				Total:   concat.Total,
				Seq:     concat.Seq,
				Content: content,
				Time:    time.Now(),
			})
		}

		fragments := s.fragmentCache[key]

		// 检查是否收集完整
		if len(fragments) == concat.Total {
			// 排序
			sort.Slice(fragments, func(i, j int) bool {
				return fragments[i].Seq < fragments[j].Seq
			})

			// 拼接
			var fullContent strings.Builder
			for _, f := range fragments {
				fullContent.WriteString(f.Content)
			}
			content = fullContent.String()

			// 清理缓存
			delete(s.fragmentCache, key)
			s.cacheMu.Unlock()

			logger.Info("长短信重组完成", "device", s.deviceID, "sender", sender, "total", concat.Total)
			// 继续处理发送逻辑...
		} else {
			s.cacheMu.Unlock()
			logger.Debug("等待更多分片...", "device", s.deviceID, "current", len(fragments), "total", concat.Total)
			return // 等待下一片
		}
	}

	if content == "" {
		content = fmt.Sprintf("[PDU 解析失败] %s", raw)
	}

	logger.Info("收到短信 (PDU)", "device", s.deviceID, "sender", sender, "content", content, "time", timestamp)

	// 通过统一通知接口转发
	if s.notifier != nil {
		if smsnotify.ShouldSuppressReceivedSMS(content) {
			logger.Info("短信已过滤（运营商 OTA/不可解码二进制包）", "device", s.deviceID, "sender", sender)
			return
		}
		s.notifier.NotifySMS(s.deviceID, sender, content, timestamp)
	}
}

// cleanupOldFragments 清理过期的短信分片 (超过10分钟)
func (s *Poller) cleanupOldFragments() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	now := time.Now()
	expiredKeys := []string{}

	for key, fragments := range s.fragmentCache {
		if len(fragments) > 0 {
			// 以第一个分片的时间为准
			if now.Sub(fragments[0].Time) > 10*time.Minute {
				expiredKeys = append(expiredKeys, key)
			}
		} else {
			expiredKeys = append(expiredKeys, key)
		}
	}

	for _, key := range expiredKeys {
		delete(s.fragmentCache, key)
		logger.Debug("清理过期短信分片", "device", s.deviceID, "key", key)
	}
}

// deleteAllMessages 删除所有短信
func (s *Poller) deleteAllMessages() {
	// AT+CMGD=1,4 模式 4 = 删除所有短信
	_, err := s.modem.ExecuteAT("AT+CMGD=1,4", 5*time.Second)
	if err != nil {
		logger.Warn("删除短信失败", "device", s.deviceID, "err", err)
	}
}

// cleanupOldSMS 清理旧短信，保留最新的 keepCount 条 (备用方法)
func (s *Poller) cleanupOldSMS(keepCount int) {
	resp, err := s.modem.ExecuteAT("AT+CMGL=4", 10*time.Second)
	if err != nil {
		return
	}

	var indices []int
	lines := strings.Split(resp, "\r\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "+CMGL:") {
			parts := strings.Split(line, ",")
			if len(parts) > 0 {
				idxStr := strings.TrimSpace(strings.TrimPrefix(parts[0], "+CMGL:"))
				if idx, err := strconv.Atoi(idxStr); err == nil {
					indices = append(indices, idx)
				}
			}
		}
	}

	if len(indices) <= keepCount {
		return
	}

	// 按索引排序，删除旧的
	sort.Ints(indices)
	deleteCount := len(indices) - keepCount
	for i := 0; i < deleteCount; i++ {
		s.modem.ExecuteAT(fmt.Sprintf("AT+CMGD=%d", indices[i]), 3*time.Second)
	}
}
