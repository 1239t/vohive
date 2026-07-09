package cscall

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/1239t/vohive/internal/modem"
	"github.com/1239t/vohive/internal/sipgw"
	"github.com/1239t/vohive/pkg/logger"
	"github.com/1239t/vowifi-go/runtimehost/voicehost"
)

// CallState 定义 CS 呼叫状态
type CallState int

const (
	CallStateIdle    CallState = iota
	CallStateRinging           // 来电振铃中（或外呼等待接通中）
	CallStateDialing           // 外呼正在拨号
	CallStateConnected
)

// Manager 负责管理 CS 域的来电并桥接到 SIP/RTP
type Manager struct {
	deviceID   string
	audioDev   string
	controller Controller
	registrar  *sipgw.Registrar

	mu               sync.Mutex
	state            CallState
	callerID         string
	sipCallID        string
	controllerCallID string
	currentCall      *CSCall

	monitorCtx    context.Context
	monitorCancel context.CancelFunc
}

// CSCall 保存当前通话相关的资源
type CSCall struct {
	audio      *AudioBridge
	clientAddr *net.UDPAddr
	clientReq  *sip.Request          // 发给客户端的 INVITE 请求（来电）
	clientTx   sip.ClientTransaction // INVITE 客户端事务（来电）
	serverTx   sip.ServerTransaction // INVITE 服务端事务（外呼，来自 Linphone 的 INVITE）
	serverReq  *sip.Request          // Linphone 发来的 INVITE 请求（外呼）
	cancelFunc context.CancelFunc    // 用于终止等待
	isOutbound bool                  // 是否是外呼
}

// NewManager 创建 CS 呼叫管理器
func NewManager(deviceID, audioDev string, m *modem.Manager, r *sipgw.Registrar) *Manager {
	return NewManagerWithController(deviceID, audioDev, NewATController(m), r)
}

func NewManagerWithController(deviceID, audioDev string, controller Controller, r *sipgw.Registrar) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	mgr := &Manager{
		deviceID:   deviceID,
		audioDev:   audioDev,
		controller: controller,
		registrar:  r,

		state:         CallStateIdle,
		monitorCtx:    ctx,
		monitorCancel: cancel,
	}

	if controller != nil {
		if err := controller.Start(ctx); err != nil {
			logger.Error(fmt.Sprintf("[%s] CSCall: 控制器启动失败", deviceID), "err", err)
		}
		go mgr.monitorControllerEvents()
		go mgr.monitorPCMReady()
	}

	return mgr
}

// Stop 停止 CS 呼叫管理器（清理内部 goroutine）
func (m *Manager) Stop() {
	if m.monitorCancel != nil {
		m.monitorCancel()
	}
	if m.controller != nil {
		m.controller.Stop()
	}
}

func (m *Manager) monitorControllerEvents() {
	if m.controller == nil {
		return
	}
	ch := m.controller.Events()
	for {
		select {
		case <-m.monitorCtx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			m.handleControllerEvent(event)
		}
	}
}

func (m *Manager) handleControllerEvent(event Event) {
	switch event.Type {
	case EventIncoming:
		m.onIncoming(event.CallID, event.Number)
	case EventHangup:
		m.onHangup()
	case EventConnected:
		m.onControllerConnected(event.CallID)
	}
}

func (m *Manager) monitorPCMReady() {
	if m.controller == nil {
		return
	}
	ch := m.controller.PCMReady()
	for {
		select {
		case <-m.monitorCtx.Done():
			return
		case ready, ok := <-ch:
			if !ok {
				return
			}
			m.mu.Lock()
			if m.currentCall != nil && m.currentCall.audio != nil {
				m.currentCall.audio.SetPCMReady(ready)
				if ready {
					logger.Debug(fmt.Sprintf("[%s] CSCall: PCM 缓冲已就绪", m.deviceID))
				} else {
					logger.Debug(fmt.Sprintf("[%s] CSCall: PCM 缓冲满，暂停传输", m.deviceID))
				}
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) beginIncomingCall(callID, number string) (sipCallID string, shouldStart bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != CallStateIdle && m.state != CallStateRinging {
		return "", false
	}
	if trimSpace(number) == "" {
		number = "Unknown"
	}
	if m.state == CallStateIdle {
		m.state = CallStateRinging
		m.callerID = number
		m.controllerCallID = callID
		m.sipCallID = uuid.NewString()
		return m.sipCallID, number != "Unknown"
	}
	if m.callerID == "Unknown" && number != "Unknown" {
		m.callerID = number
		if callID != "" {
			m.controllerCallID = callID
		}
		return m.sipCallID, true
	}
	return "", false
}

func (m *Manager) onIncoming(callID, number string) {
	sipCallID, ok := m.beginIncomingCall(callID, number)
	if ok {
		go m.initiateSIPCall(sipCallID)
		return
	}
	if number == "" || number == "Unknown" {
		if sipCallID == "" {
			m.mu.Lock()
			sipCallID = m.sipCallID
			m.mu.Unlock()
		}
		go func(callID string) {
			time.Sleep(3 * time.Second)
			m.mu.Lock()
			if m.state == CallStateRinging && m.sipCallID == callID && m.callerID == "Unknown" {
				m.mu.Unlock()
				logger.Warn(fmt.Sprintf("[%s] CSCall: 3秒未收到来电号码，使用 Unknown 发起呼叫", m.deviceID))
				m.initiateSIPCall(callID)
			} else {
				m.mu.Unlock()
			}
		}(sipCallID)
	}
}

func (m *Manager) onControllerConnected(callID string) {
	m.mu.Lock()
	if callID != "" {
		m.controllerCallID = callID
	}
	if m.state == CallStateDialing {
		m.state = CallStateConnected
	}
	m.mu.Unlock()
}

// onHangup 处理 NO CARRIER 远端挂断 URC
func (m *Manager) onHangup() {
	m.mu.Lock()
	if m.state == CallStateIdle {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	logger.Info(fmt.Sprintf("[%s] CSCall: 对端已挂断或通话中断 (NO CARRIER)", m.deviceID))
	m.endCallAndHangup(true, false) // 发送 SIP 侧戞断信号，但不需要再发 ATH
}

// initiateSIPCall 构建并向 Linphone 发起 SIP INVITE
func (m *Manager) initiateSIPCall(callID string) {
	m.mu.Lock()
	// 如果状态因为某些原因变了，取消呼叫
	if m.state != CallStateRinging || m.sipCallID != callID {
		m.mu.Unlock()
		return
	}
	caller := m.callerID
	m.mu.Unlock()

	// 先尝试发推送以唤醒 Linphone (iOS CallKit / Android FCM)
	oldContact := ""
	if existingUser := m.registrar.GetUserByDevice(m.deviceID); existingUser != nil {
		oldContact = existingUser.ContactURI
	}
	// 异步发推送唤醒 Linphone (iOS CallKit)，不阻塞主流程
	pushResultCh := make(chan bool, 1)
	go func() {
		if err := m.registrar.SendPushNotification(m.deviceID, callID, caller, ""); err != nil {
			logger.Debug(fmt.Sprintf("[%s] CSCall: 推送唤醒失败 (可能客户端已在线): %v", m.deviceID, err))
			pushResultCh <- false
		} else {
			logger.Info(fmt.Sprintf("[%s] CSCall: 已发送推送唤醒, 来电号码=%s", m.deviceID, caller))
			pushResultCh <- true
		}
	}()

	// 等待 SIP 客户端上线或用新端口重新注册
	// Push 在后台异步进行，这里同时轮询注册表
	var user *sipgw.RegisteredUser
	pushSent := false          // 是否确认推送已成功发出
	pushDone := false          // 是否已收到推送结果
	for i := 0; i < 150; i++ { // 最多等 30 秒 (150 × 200ms)
		// 非阻塞检查 Push 结果
		if !pushDone {
			select {
			case result := <-pushResultCh:
				pushSent = result
				pushDone = true
			default:
			}
		}

		user = m.registrar.GetUserByDevice(m.deviceID)
		if user != nil && user.ContactAddr != nil {
			// 如果 Push 没发出（没 token 或失败）且客户端已在线，立刻呼叫
			if pushDone && !pushSent {
				break
			}
			// Push 还没返回结果，但客户端已在线，也不用等太久
			if !pushDone && i >= 5 {
				break // 等了 1 秒了，push 还没返回，直接呼叫已有地址
			}
			// Push 成功了，检查是否有新注册
			if pushSent {
				if oldContact != "" && user.ContactURI != oldContact {
					logger.Info(fmt.Sprintf("[%s] CSCall: 检测到客户端用新端口重新注册", m.deviceID))
					break
				}
				if i >= 10 { // 等了 2 秒还没新注册，直接呼叫老地址
					logger.Debug(fmt.Sprintf("[%s] CSCall: 2秒未收到新注册，尝试呼叫已知地址", m.deviceID))
					break
				}
			}
		} else {
			// 客户端完全未注册
			if i == 0 {
				logger.Info(fmt.Sprintf("[%s] CSCall: 来电 %s，等待 SIP 客户端上线...", m.deviceID, caller))
			}
		}

		time.Sleep(200 * time.Millisecond)
		// 检查来电是否已被取消
		m.mu.Lock()
		if m.state != CallStateRinging || m.sipCallID != callID {
			m.mu.Unlock()
			logger.Info(fmt.Sprintf("[%s] CSCall: 等待客户端期间来电已结束", m.deviceID))
			return
		}
		m.mu.Unlock()
	}
	if user == nil {
		logger.Warn(fmt.Sprintf("[%s] CSCall: 等待 30 秒后仍无 SIP 客户端上线，挂断来电", m.deviceID))
		m.endCallAndHangup(false, true)
		return
	}

	// 最后一次刷新确保拿到最新的 Contact 地址
	if latest := m.registrar.GetUserByDevice(m.deviceID); latest != nil {
		user = latest
	}

	// 准备 AudioBridge，提前绑定本地端口以便塞入 SDP
	ab, err := NewAudioBridge(m.audioDev, m.deviceID)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: 初始化 AudioBridge 失败", m.deviceID), "err", err)
		m.endCallAndHangup(false, true)
		return
	}

	call := &CSCall{
		audio: ab,
	}

	m.mu.Lock()
	m.currentCall = call
	m.mu.Unlock()

	// 构造本地 SDP (仅 G.711 μ-law)
	// 确定本机 IP：通过 UDP 出站探测，而不是依赖配置
	localIP := m.detectLocalIP(user.ContactAddr.IP.String())
	if localIP == "" {
		localIP = m.registrar.GetExternalIP()
	}
	if localIP == "" {
		localIP = "127.0.0.1" // 最后 Fallback
	}

	// 这里复用 voice 包的逻辑，但自己构造一个极简 SDP
	sdpStr := m.buildLocalSDP(localIP, ab.LocalPort())

	// 准备 SIP 请求
	from := &sip.Uri{User: caller, Host: "cscall.vohive"}
	to := &sip.Uri{User: user.Username, Host: localIP}
	contact := &sip.Uri{User: caller, Host: localIP}

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: user.Username, Host: user.ContactAddr.IP.String(), Port: user.ContactAddr.Port})
	req.SetDestination(user.ContactAddr.String())
	if user.Transport != "" {
		req.SetTransport(user.Transport)
	}
	req.AppendHeader(sip.NewHeader("From", "<"+from.String()+fmt.Sprintf(">;tag=%s", uuid.NewString()[:8])))
	req.AppendHeader(sip.NewHeader("To", "<"+to.String()+">"))
	req.AppendHeader(sip.NewHeader("Call-ID", callID))
	req.AppendHeader(sip.NewHeader("CSeq", "1 INVITE"))
	req.AppendHeader(sip.NewHeader("Contact", "<"+contact.String()+">"))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody([]byte(sdpStr))

	logger.Debug(fmt.Sprintf("[%s] CSCall: INVITE 目标=%s, From=%s, To=%s, SDP_IP=%s, SDP_RTP=%d",
		m.deviceID, user.ContactAddr.String(), caller, user.Username, localIP, ab.LocalPort()))

	// 使用 registrar 的 UA 发送
	tx, err := m.registrar.GetClient().TransactionRequest(context.Background(), req)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: 发送 SIP INVITE 失败", m.deviceID), "err", err)
		ab.Stop()
		m.endCallAndHangup(false, true)
		return
	}

	call.clientReq = req
	call.clientTx = tx

	ctx, cancel := context.WithCancel(context.Background())
	call.cancelFunc = cancel

	logger.Info(fmt.Sprintf("[%s] CSCall: 已向客户端 %s 发送 INVITE, 等待接听", m.deviceID, user.Username))

	// 等待响应
	go m.waitClientResponse(ctx, call, callID)
}

// detectLocalIP 通过 UDP 出站探测获取本机到达目标 IP 时使用的源地址
func (m *Manager) detectLocalIP(targetIP string) string {
	conn, err := net.Dial("udp", targetIP+":1")
	if err != nil {
		return ""
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func (m *Manager) buildLocalSDP(ip string, port int) string {
	// SDP 提供 PCMU(0) 编解码
	// o= 字段中的 session-id 和 version 必须是数字 (RFC 4566)
	sessionID := fmt.Sprintf("%d", time.Now().UnixNano())
	sessionVersion := fmt.Sprintf("%d", rand.Int31())
	return fmt.Sprintf(
		"v=0\r\n"+
			"o=- %s %s IN IP4 %s\r\n"+
			"s=VoHive CS Call\r\n"+
			"c=IN IP4 %s\r\n"+
			"t=0 0\r\n"+
			"m=audio %d RTP/AVP 0\r\n"+
			"a=rtpmap:0 PCMU/8000\r\n"+
			"a=ptime:20\r\n"+
			"a=sendrecv\r\n",
		sessionID, sessionVersion, ip, ip, port)
}

func (m *Manager) waitClientResponse(ctx context.Context, call *CSCall, callID string) {
	for {
		select {
		case <-ctx.Done():
			return
		case res, ok := <-call.clientTx.Responses():
			if !ok {
				logger.Warn(fmt.Sprintf("[%s] CSCall: Transaction 已关闭", m.deviceID))
				m.endCallAndHangup(true, true)
				return
			}

			code := res.StatusCode
			logger.Debug(fmt.Sprintf("[%s] CSCall: 收到客户端响应 %d", m.deviceID, code))

			if code >= 200 && code < 300 {
				// 客户端接听了 (200 OK)
				m.handleClientAnswer(call, callID, res)
				return
			} else if code >= 300 {
				// 客户端拒绝或忙
				logger.Info(fmt.Sprintf("[%s] CSCall: 客户端拒绝接听 (%d)", m.deviceID, code))
				m.endCallAndHangup(false, true)
				return
			}
			// 1xx 响应，继续等待
		}
	}
}

func (m *Manager) handleClientAnswer(call *CSCall, callID string, res *sip.Response) {
	m.mu.Lock()
	if m.state != CallStateRinging || m.sipCallID != callID {
		m.mu.Unlock()
		return
	}
	m.state = CallStateConnected
	m.mu.Unlock()

	logger.Info(fmt.Sprintf("[%s] CSCall: 客户端已接听，开始建立媒体通道", m.deviceID))

	// 解析 SDP 获取远端 RTP 地址
	sdpInfo, err := voicehost.ParseSDP(res.Body())
	if err == nil {
		call.clientAddr = &net.UDPAddr{
			IP:   net.ParseIP(sdpInfo.ConnectionIP),
			Port: sdpInfo.MediaPort,
		}
		call.audio.SetClientAddr(sdpInfo.ConnectionIP, sdpInfo.MediaPort)
	} else {
		logger.Warn(fmt.Sprintf("[%s] CSCall: 解析协商 SDP 失败, 回退到通过第一个 RTP 包学习其地址", m.deviceID), "err", err)
	}

	// 构造 ACK - RFC 3261 §13.2.2.4
	// Request-URI: 使用 Contact 的 host:port，但精简掉 pn-* 推送参数（避免超 MTU）
	var ackRecipient sip.Uri
	if contactHdr := res.Contact(); contactHdr != nil {
		ackRecipient = sip.Uri{
			Scheme: contactHdr.Address.Scheme,
			User:   contactHdr.Address.User,
			Host:   contactHdr.Address.Host,
			Port:   contactHdr.Address.Port,
		}
	} else {
		ackRecipient = sip.Uri{
			Scheme: call.clientReq.Recipient.Scheme,
			User:   call.clientReq.Recipient.User,
			Host:   call.clientReq.Recipient.Host,
			Port:   call.clientReq.Recipient.Port,
		}
	}

	ack := sip.NewRequest(sip.ACK, ackRecipient)
	ack.SipVersion = call.clientReq.SipVersion

	// Via: 新的 branch（ACK for 2xx 是独立请求）
	origVia := call.clientReq.Via()
	viaHop := &sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "UDP",
		Host:            origVia.Host,
		Port:            origVia.Port,
		Params:          sip.NewParams(),
	}
	viaHop.Params.Add("branch", sip.GenerateBranch())
	ack.AppendHeader(viaHop)

	// Route（如有）
	if len(call.clientReq.GetHeaders("Route")) > 0 {
		sip.CopyHeaders("Route", call.clientReq, ack)
	}
	// From: 来自原始 INVITE 请求（含 from-tag）
	if h := call.clientReq.From(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	// To: 来自 200 OK（含 to-tag）
	if h := res.To(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	// Call-ID
	if h := call.clientReq.CallID(); h != nil {
		ack.AppendHeader(sip.HeaderClone(h))
	}
	// CSeq: 从 INVITE 克隆，改 Method 为 ACK
	if h := call.clientReq.CSeq(); h != nil {
		cseqClone := sip.HeaderClone(h).(*sip.CSeqHeader)
		cseqClone.MethodName = sip.ACK
		ack.AppendHeader(cseqClone)
	}
	maxFwd := sip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxFwd)
	ack.SetBody(nil)
	ack.SetTransport(call.clientReq.Transport())
	ack.SetSource(call.clientReq.Source())
	ack.SetDestination(call.clientReq.Destination())

	logger.Debug(fmt.Sprintf("[%s] CSCall: 即将发送的 ACK 原始报文:\n%s", m.deviceID, ack.String()))

	if err := m.registrar.GetClient().WriteRequest(ack, sipgo.ClientRequestBuild); err != nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: 发送 ACK 失败", m.deviceID), "err", err)
	} else {
		logger.Debug(fmt.Sprintf("[%s] CSCall: ACK 已发送到 %s", m.deviceID, call.clientReq.Destination()))
	}

	// 启动 AudioBridge
	if err := call.audio.Start(); err != nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: AudioBridge 启动失败", m.deviceID), "err", err)
		m.endCallAndHangup(true, true)
		return
	}

	if m.controller == nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: 控制面未初始化，无法接听", m.deviceID))
		m.endCallAndHangup(true, false)
		return
	}
	if err := m.controller.Answer(context.Background(), m.controllerCallID); err != nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: 接听指令失败", m.deviceID), "err", err)
		m.endCallAndHangup(true, true)
		return
	}

	// 既然免去了动态 QPCMV 配置，这里必须主动将 PCM 状态置为就绪，
	// 否则上行 RTP 收到的音频因为 state == false 会被静音/丢弃。
	call.audio.SetPCMReady(true)

	logger.Info(fmt.Sprintf("[%s] CSCall: 双向语音建立完成", m.deviceID))
}

// HasCall 检查是否正在处理具有特定 Call-ID 的呼叫
func (m *Manager) HasCall(callID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sipCallID == callID && m.state != CallStateIdle
}

// HandleClientCancel 处理客户端拒接呼叫
func (m *Manager) HandleClientCancel(callID string) bool {
	m.mu.Lock()
	if m.sipCallID != callID || m.state != CallStateRinging {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	logger.Info(fmt.Sprintf("[%s] CSCall: 客户端拒接呼叫 (CANCEL)", m.deviceID))
	m.endCallAndHangup(false, true) // 客户端手动拒接（CANCEL），需要发 ATH 挂断真实来电
	return true
}

// HandleClientBye 处理来自客户端的主动挂断
func (m *Manager) HandleClientBye(callID string) bool {
	m.mu.Lock()
	if m.sipCallID != callID || m.state == CallStateIdle {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	logger.Info(fmt.Sprintf("[%s] CSCall: 客户端主动挂断", m.deviceID))
	m.endCallAndHangup(false, true) // 不需要再发 BYE 给客户端，但需要 ATH
	return true
}

// HandleOutboundInvite 处理来自 Linphone 的外呼 INVITE
func (m *Manager) HandleOutboundInvite(deviceID string, req *sip.Request, tx sip.ServerTransaction) {
	to := req.To()
	if to == nil {
		tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request - Missing To", nil))
		return
	}
	callee := to.Address.User
	callID := req.CallID().Value()

	// 生成我们的 To tag，并附加到 req，以便 NewResponseFromRequest 自动带出
	toTag := uuid.NewString()[:8]
	to.Params.Add("tag", toTag)

	logger.Info(fmt.Sprintf("[%s] CSCall: 收到 Linphone 外呼请求, 被叫=%s", m.deviceID, callee))

	m.mu.Lock()
	if m.state != CallStateIdle {
		m.mu.Unlock()
		logger.Warn(fmt.Sprintf("[%s] CSCall: 外呼失败，当前有通话进行中", m.deviceID))
		tx.Respond(sip.NewResponseFromRequest(req, 486, "Busy Here", nil))
		return
	}
	m.state = CallStateDialing
	m.sipCallID = callID
	m.callerID = callee
	m.mu.Unlock()

	// 准备 AudioBridge
	localIP := m.registrar.GetExternalIP()
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	ab, err := NewAudioBridge(m.audioDev, m.deviceID)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: 初始化 AudioBridge 失败", m.deviceID), "err", err)
		tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		m.mu.Lock()
		m.state = CallStateIdle
		m.mu.Unlock()
		return
	}

	call := &CSCall{
		audio:      ab,
		serverTx:   tx,
		serverReq:  req,
		isOutbound: true,
	}
	m.mu.Lock()
	m.currentCall = call
	m.mu.Unlock()

	// 解析 Linphone SDP 中的 RTP 客户端地址
	if body := req.Body(); len(body) > 0 {
		if sdpInfo, err := voicehost.ParseSDP(body); err == nil {
			ab.SetClientAddr(sdpInfo.ConnectionIP, sdpInfo.MediaPort)
		}
	}

	if m.controller == nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: 控制面未初始化，无法拨号", m.deviceID))
		tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable - Call Control Missing", nil))
		m.mu.Lock()
		m.state = CallStateIdle
		m.currentCall = nil
		m.mu.Unlock()
		ab.Stop()
		return
	}

	// 发起蜂窝拨号
	logger.Info(fmt.Sprintf("[%s] CSCall: 正在拨打 %s...", m.deviceID, callee))
	ref, err := m.controller.Dial(context.Background(), callee)
	if err != nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: 蜂窝拨号失败", m.deviceID), "err", err)
		tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable - Dial Failed", nil))
		m.mu.Lock()
		m.state = CallStateIdle
		m.currentCall = nil
		m.mu.Unlock()
		ab.Stop()
		return
	}
	m.mu.Lock()
	m.controllerCallID = ref.ID
	m.mu.Unlock()

	// ATD 成功（模组返回 OK）说明拨号已发出，向 Linphone 回复 180 Ringing
	res180 := sip.NewResponseFromRequest(req, 180, "Ringing", nil)
	tx.Respond(res180)
	logger.Debug(fmt.Sprintf("[%s] CSCall: 已向 Linphone 发送 180 Ringing", m.deviceID))

	// 等待对方接听：通过 +CLCC 轮询检测通话状态
	// AT+CLCC 返回的 stat 字段: 0=active(接通), 2=dialing, 3=alerting(振铃中)
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer connectCancel()

	call.cancelFunc = connectCancel
	connected := false

	for {
		select {
		case <-connectCtx.Done():
			logger.Warn(fmt.Sprintf("[%s] CSCall: 外呼超时，对方未接听", m.deviceID))
			tx.Respond(sip.NewResponseFromRequest(req, 408, "Request Timeout", nil))
			m.endCallAndHangup(false, true)
			return
		default:
		}

		// 检查通话是否已被取消
		m.mu.Lock()
		if m.state == CallStateIdle {
			m.mu.Unlock()
			return // 已经被 onHangup 或其他逻辑清理了
		}
		m.mu.Unlock()

		time.Sleep(500 * time.Millisecond)

		calls, err := m.controller.GetCalls(connectCtx)
		if err != nil {
			continue
		}
		if hasConnectedCall(calls, m.controllerCallID) {
			connected = true
			break
		}
	}

	if !connected {
		return
	}

	logger.Info(fmt.Sprintf("[%s] CSCall: 对方已接听，建立媒体通道", m.deviceID))

	m.mu.Lock()
	m.state = CallStateConnected
	m.mu.Unlock()

	// 构造 200 OK + SDP
	sdpStr := m.buildLocalSDP(localIP, ab.LocalPort())
	res200 := sip.NewResponseFromRequest(req, 200, "OK", []byte(sdpStr))
	res200.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	// 在 200 OK 中必须携带 Contact 头，否则客户端无法发送 ACK
	contact := &sip.Uri{User: callee, Host: localIP}
	res200.AppendHeader(sip.NewHeader("Contact", "<"+contact.String()+">"))

	tx.Respond(res200)
	logger.Debug(fmt.Sprintf("[%s] CSCall: 已向 Linphone 发送 200 OK (含 SDP, Contact=%s)", m.deviceID, contact.String()))

	// 启动 AudioBridge
	if err := ab.Start(); err != nil {
		logger.Error(fmt.Sprintf("[%s] CSCall: AudioBridge 启动失败", m.deviceID), "err", err)
		m.endCallAndHangup(false, true)
		return
	}
	ab.SetPCMReady(true)

	logger.Info(fmt.Sprintf("[%s] CSCall: 外呼双向语音建立完成, 被叫=%s", m.deviceID, callee))
}

// containsCLCCActive 检查 AT+CLCC 返回中是否有外呼语音通话已接通
// +CLCC: idx,dir,stat,mode,mpty[,"number",type]
// dir=0(MO 外呼), stat=0(active 已接通), mode=0(voice 语音)
// 必须三个条件同时满足，排除 VoLTE 数据承载 (mode=1) 的干扰
func containsCLCCActive(resp string) bool {
	for _, line := range splitLines(resp) {
		if len(line) < 7 {
			continue
		}
		idx := 0
		for i, ch := range line {
			if ch == ':' {
				idx = i + 1
				break
			}
		}
		if idx == 0 {
			continue
		}
		fields := splitCSV(line[idx:])
		if len(fields) >= 4 {
			dir := trimSpace(fields[1])
			stat := trimSpace(fields[2])
			mode := trimSpace(fields[3])
			// dir=0(MO) + stat=0(active) + mode=0(voice)
			if dir == "0" && stat == "0" && mode == "0" {
				return true
			}
		}
	}
	return false
}

func hasConnectedCall(calls []CallInfo, callID string) bool {
	for _, call := range calls {
		if call.State != CallStateConnected {
			continue
		}
		if callID == "" || call.ID == "" || call.ID == callID {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func splitCSV(s string) []string {
	var fields []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			fields = append(fields, s[start:i])
			start = i + 1
		}
	}
	fields = append(fields, s[start:])
	return fields
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}

func (m *Manager) endCallAndHangup(sendClientSignal bool, sendATH bool) {
	m.mu.Lock()
	if m.state == CallStateIdle {
		m.mu.Unlock()
		return
	}
	stateSnapshot := m.state // 快照当前状态，稍后判断发 CANCEL 还是发 BYE
	m.state = CallStateIdle
	call := m.currentCall
	m.currentCall = nil
	m.mu.Unlock()

	logger.Info(fmt.Sprintf("[%s] CSCall: 挂断并清理通话", m.deviceID))

	if m.controller != nil {
		_ = m.controller.Hangup(context.Background(), m.controllerCallID, HangupOptions{SendModemSignal: sendATH})
	}

	if call != nil {
		if call.cancelFunc != nil {
			call.cancelFunc()
		}
		if call.audio != nil {
			call.audio.Stop()
		}

		if call.isOutbound {
			// 外呼场景：向 Linphone 发 BYE（如果已接通）
			if sendClientSignal && call.serverReq != nil {
				if stateSnapshot == CallStateConnected {
					bye := sip.NewRequest(sip.BYE, call.serverReq.From().Address)
					sip.CopyHeaders("To", call.serverReq, bye)
					sip.CopyHeaders("From", call.serverReq, bye)
					sip.CopyHeaders("Call-ID", call.serverReq, bye)
					bye.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d BYE", call.serverReq.CSeq().SeqNo+1)))
					m.registrar.GetClient().WriteRequest(bye)
				}
				// 拨号中对方未接：serverTx 上的 408/487 已经在调用方发送了
			}
		} else if sendClientSignal && call.clientReq != nil && call.clientTx != nil {
			// 来电场景（原有逻辑）
			if stateSnapshot == CallStateRinging {
				cancel := sip.NewRequest(sip.CANCEL, call.clientReq.Recipient)
				sip.CopyHeaders("To", call.clientReq, cancel)
				sip.CopyHeaders("From", call.clientReq, cancel)
				sip.CopyHeaders("Call-ID", call.clientReq, cancel)
				sip.CopyHeaders("Via", call.clientReq, cancel)
				cancel.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d CANCEL", call.clientReq.CSeq().SeqNo)))
				m.registrar.GetClient().WriteRequest(cancel)
			} else {
				bye := sip.NewRequest(sip.BYE, call.clientReq.Recipient)
				sip.CopyHeaders("To", call.clientReq, bye)
				sip.CopyHeaders("From", call.clientReq, bye)
				sip.CopyHeaders("Call-ID", call.clientReq, bye)
				bye.AppendHeader(sip.NewHeader("CSeq", fmt.Sprintf("%d BYE", call.clientReq.CSeq().SeqNo+1)))
				m.registrar.GetClient().WriteRequest(bye)
			}
		}
	}
}
