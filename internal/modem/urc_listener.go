package modem

import (
	"bufio"
	"strings"
	"sync"
	"time"

	"github.com/1239t/vohive/pkg/logger"

	"go.bug.st/serial"
)

// URCHandler 定义 URC 消息的回调处理函数
type URCHandler func(urc string, params []string)

// URCListener 在辅助 AT 端口上监听模组的主动上报消息 (URC)
// 常见 URC:
//   - +CMTI: "SM",1     新短信到达 (存储位置, 索引)
//   - +CREG: 1          网络注册状态变化
//   - +CPIN: READY      SIM 卡就绪
//   - +CSQ: 20,99       信号强度变化 (需要先启用 AT+CSQR)
type URCListener struct {
	deviceID string
	auxPort  string // 辅助 AT 端口路径 (如 /dev/ttyUSB3)

	conn    serial.Port
	running bool
	mu      sync.Mutex

	handlers map[string]URCHandler // 按 URC 类型注册的处理器
}

// NewURCListener 创建一个新的 URC 监听器
func NewURCListener(deviceID, auxPort string) *URCListener {
	return &URCListener{
		deviceID: deviceID,
		auxPort:  auxPort,
		handlers: make(map[string]URCHandler),
	}
}

// RegisterHandler 为指定的 URC 类型注册处理函数
// urcType 是 URC 前缀，如 "+CMTI", "+CREG"
func (u *URCListener) RegisterHandler(urcType string, handler URCHandler) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handlers[urcType] = handler
}

// Start 开始监听 URC 消息 (阻塞式，应在 goroutine 中调用)
func (u *URCListener) Start() error {
	if u.auxPort == "" {
		logger.Warn("URC 监听器未启动: 未配置辅助 AT 端口", "device", u.deviceID)
		return nil
	}

	logger.Info("启动 URC 监听器", "device", u.deviceID, "port", u.auxPort)

	// 打开串口
	mode := &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		StopBits: serial.OneStopBit,
		Parity:   serial.NoParity,
	}

	var err error
	u.conn, err = serial.Open(u.auxPort, mode)
	if err != nil {
		return err
	}

	u.running = true

	// 启用常用的 URC 报告
	u.enableURCReporting()

	// 开始读取循环
	reader := bufio.NewReader(u.conn)
	for u.running {
		// 设置读取超时，允许定期检查 running 状态
		u.conn.SetReadTimeout(1 * time.Second)

		line, err := reader.ReadString('\n')
		if err != nil {
			// 超时或连接断开，继续循环
			continue
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 解析 URC
		u.parseAndDispatch(line)
	}

	return nil
}

// Stop 停止监听
func (u *URCListener) Stop() {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.running = false
	if u.conn != nil {
		u.conn.Close()
		u.conn = nil
	}
}

// enableURCReporting 启用模组的 URC 主动上报功能
func (u *URCListener) enableURCReporting() {
	// 发送命令启用各类 URC
	commands := []string{
		"ATE0",              // 关闭回显
		"AT+CMGF=0",         // PDU 模式 (更可靠)
		"AT+CNMI=2,1,0,0,0", // 新短信上报 +CMTI
	}

	for _, cmd := range commands {
		u.sendCommand(cmd)
		time.Sleep(100 * time.Millisecond)
	}

	logger.Info("URC 上报模式已启用", "device", u.deviceID)
}

// sendCommand 发送 AT 命令 (不等待复杂响应)
func (u *URCListener) sendCommand(cmd string) {
	if u.conn == nil {
		return
	}
	_, _ = u.conn.Write([]byte(cmd + "\r\n"))
}

// parseAndDispatch 解析 URC 并分发给对应的处理器
func (u *URCListener) parseAndDispatch(line string) {
	// URC 格式通常是: +TYPE: param1,param2,...
	// 例如: +CMTI: "SM",1

	if !strings.HasPrefix(line, "+") {
		return // 不是 URC
	}

	colonIdx := strings.Index(line, ":")
	if colonIdx == -1 {
		// 无参数的 URC，如 RING
		u.dispatch(line, nil)
		return
	}

	urcType := line[:colonIdx]
	paramsStr := strings.TrimSpace(line[colonIdx+1:])

	// 解析参数
	var params []string
	if paramsStr != "" {
		// 简单分割 (不处理引号内的逗号，需要复杂解析时可扩展)
		params = strings.Split(paramsStr, ",")
		for i := range params {
			params[i] = strings.Trim(strings.TrimSpace(params[i]), "\"")
		}
	}

	u.dispatch(urcType, params)
}

// dispatch 调用对应的处理器
func (u *URCListener) dispatch(urcType string, params []string) {
	u.mu.Lock()
	handler, ok := u.handlers[urcType]
	u.mu.Unlock()

	if ok {
		logger.Debug("收到 URC", "device", u.deviceID, "type", urcType, "params", params)
		handler(urcType, params)
	} else {
		// 未注册的 URC，仅记录 debug 日志
		logger.Debug("收到未处理的 URC", "device", u.deviceID, "type", urcType, "params", params)
	}
}
