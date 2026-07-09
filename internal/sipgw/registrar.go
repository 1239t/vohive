// Package voice 提供 VoWiFi 语音通话功能
package sipgw

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/1239t/vohive/pkg/logger"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// 预编译 Push 通知参数解析用正则表达式
var (
	rePnPrid        = regexp.MustCompile(`pn-prid=([^;]+)`)
	rePnProvider    = regexp.MustCompile(`pn-provider=([^;]+)`)
	rePnParam       = regexp.MustCompile(`pn-param=([^;]+)`)
	rePnCallStr     = regexp.MustCompile(`pn-call-str=([^;]+)`)
	rePnMsgStr      = regexp.MustCompile(`pn-msg-str=([^;]+)`)
	reSanitizeParam = regexp.MustCompile(`[^a-zA-Z0-9._]`)
	reSanitizeToken = regexp.MustCompile(`[^a-zA-Z0-9\-_:]`)
)

type RegisteredUser struct {
	Username    string       // 用户名
	DeviceID    string       // 绑定的设备 ID
	DisplayName string       // 显示名称
	ContactURI  string       // Linphone 的 Contact URI
	ContactAddr *net.UDPAddr // Linphone 地址 (UDP)
	Source      string       // 源地址 (IP:Port)
	Transport   string       // 传输协议
	Expires     time.Time    // 过期时间
	UserAgent   string       // User-Agent

	// Push Notification Info (从 Contact 收集)
	PushToken    string // pn-prid
	PushProvider string // pn-provider (apns, fcm)
	PushParam    string // pn-param
	PushCallStr  string // pn-call-str (如 IC_MSG 等触发字段)
	PushMsgStr   string // pn-msg-str
}

// Registrar Linphone SIP 注册服务
// 接受 Linphone 客户端的 REGISTER 请求，维护在线用户列表
type Registrar struct {
	cfg    Config
	ua     *sipgo.UserAgent
	client *sipgo.Client // SIP Client 用于发送 INVITE
	server *sipgo.Server

	mu            sync.RWMutex
	users         map[string]*RegisteredUser // username -> user
	byDevice      map[string]*RegisteredUser // deviceID -> user
	nonces        map[string]time.Time       // 认证 nonce -> 过期时间
	onlineSignals map[string]chan struct{}   // deviceID -> closed when device becomes online, then replaced

	// 呼出回调：Linphone 发起 INVITE 时调用
	onInvite func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	// 取消回调
	onCancel func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	// PRACK 回调
	onPrack func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	// ACK 回调
	onAck func(deviceID string, req *sip.Request, tx sip.ServerTransaction)
	// BYE 回调：Linphone 挂断时调用
	onBye func(deviceID string, req *sip.Request, tx sip.ServerTransaction)

	// 并发控制信号量
	concurrencySem chan struct{}

	pushAuthCache map[string]string
	pushAuthNC    uint32
	pushAuthMu    sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
}

// NewRegistrar 创建 Linphone 注册服务
func NewRegistrar(cfg Config) (*Registrar, error) {
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent("VoHive/1.0"),
	)
	if err != nil {
		return nil, fmt.Errorf("创建 SIP UserAgent 失败: %w", err)
	}

	return &Registrar{
		cfg:            cfg,
		ua:             ua,
		users:          make(map[string]*RegisteredUser),
		byDevice:       make(map[string]*RegisteredUser),
		nonces:         make(map[string]time.Time),
		onlineSignals:  make(map[string]chan struct{}),
		concurrencySem: make(chan struct{}, 100), // 默认并发限制 100
		pushAuthCache:  make(map[string]string),
	}, nil
}

// GetClient 获取 SIP Client (用于 Agent 发送 INVITE)
func (r *Registrar) GetClient() *sipgo.Client {
	return r.client
}

// GetUA 获取 UserAgent
func (r *Registrar) GetUA() *sipgo.UserAgent {
	return r.ua
}

// GetExternalIP 返回配置的公网 IP
func (r *Registrar) GetExternalIP() string {
	return r.cfg.SIP.ExternalIP
}

// GetListenAddr 返回配置的监听地址
func (r *Registrar) GetListenAddr() string {
	return r.cfg.SIP.Listen
}

// RTPPortRange 返回软电话侧媒体端口范围。
func (r *Registrar) RTPPortRange() (int, int) {
	return r.cfg.Media.RTPPortMin, r.cfg.Media.RTPPortMax
}

func (r *Registrar) CountRegistered() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byDevice)
}

// Start 启动注册服务
func (r *Registrar) Start(ctx context.Context) error {
	r.ctx, r.cancel = context.WithCancel(ctx)

	srv, err := sipgo.NewServer(r.ua)
	if err != nil {
		return fmt.Errorf("创建 SIP Server 失败: %w", err)
	}
	r.server = srv

	// 注册 SIP 处理器 (必须在 ListenAndServe 之前)
	srv.OnRegister(r.handleRegister)
	srv.OnInvite(r.handleInvite)            // 呼出 INVITE
	srv.OnAck(r.handleAck)                  // ACK
	srv.OnBye(r.handleBye)                  // 挂断
	srv.OnRequest("CANCEL", r.handleCancel) // 取消呼叫 (sipgo fix)
	// srv.OnCancel(r.handleCancel) // OnCancel acts weirdly on some versions, use OnMsg

	// Linphone 注册后会发送 PUBLISH（在线状态），直接回 200 OK 即可
	srv.OnRequest("PUBLISH", func(req *sip.Request, tx sip.ServerTransaction) {
		r.respond(tx, req, 200, "OK")
	})

	// 处理 PRACK (用于 100rel)
	srv.OnRequest("PRACK", r.handlePrack)

	// 创建 SIP Client (用于发送 INVITE)
	// 注意: 必须在 Server 注册处理器之后创建，否则 Client 会拦截入站请求
	listenHost, listenPortStr, err := net.SplitHostPort(r.cfg.SIP.Listen)
	if err != nil {
		return fmt.Errorf("解析 SIP.Listen 失败: %w", err)
	}
	listenPort, err := strconv.Atoi(listenPortStr)
	if err != nil {
		return fmt.Errorf("解析 SIP.Listen 端口失败: %w", err)
	}

	clientHost := r.cfg.SIP.ExternalIP
	if clientHost == "" {
		switch listenHost {
		case "", "0.0.0.0", "::":
			if addrs, addrErr := net.InterfaceAddrs(); addrErr == nil {
				for _, addr := range addrs {
					if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
						if ip4 := ipnet.IP.To4(); ip4 != nil {
							clientHost = ip4.String()
							break
						}
					}
				}
			}
		default:
			clientHost = listenHost
		}
	}
	if clientHost == "" {
		clientHost = "127.0.0.1"
	}

	client, err := sipgo.NewClient(
		r.ua,
		sipgo.WithClientHostname(clientHost),
		sipgo.WithClientPort(listenPort),
		sipgo.WithClientNAT(),
	)
	if err != nil {
		return fmt.Errorf("创建 SIP Client 失败: %w", err)
	}
	r.client = client

	// 启动监听 (UDP 和 TCP 同时支持)
	addr := r.cfg.SIP.Listen

	logger.Info("启动 Linphone SIP 注册服务", "addr", addr, "transport", "udp+tcp", "realm", r.cfg.SIP.Realm)

	// 启动 UDP 监听
	go func() {
		if err := srv.ListenAndServe(r.ctx, "udp", addr); err != nil {
			if r.ctx.Err() == nil {
				logger.Error("SIP UDP 监听失败", "err", err)
			}
		}
	}()

	// 启动 TCP 监听
	go func() {
		if err := srv.ListenAndServe(r.ctx, "tcp", addr); err != nil {
			if r.ctx.Err() == nil {
				logger.Error("SIP TCP 监听失败", "err", err)
			}
		}
	}()

	// 定期清理过期注册
	go r.cleanupLoop()

	return nil
}

// Stop 停止注册服务
func (r *Registrar) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	logger.Info("Linphone 注册服务已停止")
	return nil
}

// handleRegister 处理 REGISTER 请求
func (r *Registrar) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	logger.RunDebug("handleRegister 被调用", "remote", req.Source())

	// 1. 提取 From 用户名
	from := req.From()
	if from == nil {
		r.respond(tx, req, 400, "Bad Request - Missing From")
		return
	}
	username := from.Address.User
	if username == "" {
		r.respond(tx, req, 400, "Bad Request - Missing From User")
		return
	}

	deviceID := ""
	if cfg := r.findUserConfig(username); cfg != nil {
		deviceID = cfg.DeviceID
	}
	logger.RunDebug("收到 REGISTER 请求", "username", username, "device", deviceID, "from", from.String())

	// 2. 检查 Authorization
	authHeader := req.GetHeader("Authorization")
	if authHeader == nil {
		// 发送 401 挑战
		r.sendAuthChallenge(tx, req)
		return
	}

	// 3. 验证认证
	if !r.validateAuth(username, authHeader.Value()) {
		logger.WarnRate("registrar_register_auth_failed:"+username+":"+deviceID, 60*time.Second,
			"REGISTER 认证失败",
			"username", username,
			"device", deviceID,
		)
		r.sendAuthChallenge(tx, req)
		return
	}

	// 4. 检查用户配置
	userCfg := r.findUserConfig(username)
	if userCfg == nil {
		logger.WarnRate("registrar_register_user_missing:"+username+":"+deviceID, 60*time.Second,
			"REGISTER 用户未配置",
			"username", username,
			"device", deviceID,
		)
		r.respond(tx, req, 403, "Forbidden - User not configured")
		return
	}

	// 5. 提取 Contact 和 Expires
	contact := req.Contact()
	expires := r.parseExpires(req)

	if expires == 0 {
		// 注销
		r.unregisterUser(username)
		r.respond(tx, req, 200, "OK")
		logger.Info("Linphone 注销成功", "username", username, "device", userCfg.DeviceID)
		return
	}

	// 6. 注册用户
	contactURI := ""
	if contact != nil {
		contactURI = contact.Address.String()
	}

	userAgent := ""
	if uaHeader := req.GetHeader("User-Agent"); uaHeader != nil {
		userAgent = uaHeader.Value()
	}

	var contactAddr *net.UDPAddr
	if src := fmt.Sprint(req.Source()); src != "" && src != "<nil>" {
		if addr, err := net.ResolveUDPAddr("udp", src); err == nil {
			contactAddr = addr
		}
	}

	// 提取 Push 通知参数 (改用正则从原始字符串硬提取，防止 sipgo 解析参数丢弃)
	pushToken, pushProvider, pushParam := "", "", ""
	pushCallStr, pushMsgStr := "", ""
	if contact != nil {
		rawContact := contact.String()

		if matches := rePnPrid.FindStringSubmatch(rawContact); len(matches) > 1 {
			pushToken = matches[1]
		}
		if matches := rePnProvider.FindStringSubmatch(rawContact); len(matches) > 1 {
			pushProvider = matches[1]
		}
		if matches := rePnParam.FindStringSubmatch(rawContact); len(matches) > 1 {
			pushParam = matches[1]
		}
		if matches := rePnCallStr.FindStringSubmatch(rawContact); len(matches) > 1 {
			pushCallStr = matches[1]
		}
		if matches := rePnMsgStr.FindStringSubmatch(rawContact); len(matches) > 1 {
			pushMsgStr = matches[1]
		}

		// 净化参数以满足 Linphone 推送网关的合法性拦截检查
		if idx := strings.Index(pushParam, "&"); idx != -1 {
			pushParam = pushParam[:idx]
		}
		if idx := strings.Index(pushToken, "&"); idx != -1 {
			pushToken = pushToken[:idx]
		}
		// 使用预编译正则扫除非法字符
		pushParam = reSanitizeParam.ReplaceAllString(pushParam, "")
		pushToken = reSanitizeToken.ReplaceAllString(pushToken, "")
	}

	r.registerUser(username, userCfg.DeviceID, userCfg.DisplayName, contactURI, contactAddr, req.Transport(), userAgent, expires, pushToken, pushProvider, pushParam, pushCallStr, pushMsgStr)

	// 7. 发送 200 OK
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	if contact != nil {
		res.AppendHeader(contact)
	}
	res.AppendHeader(sip.NewHeader("Expires", fmt.Sprintf("%d", expires)))
	tx.Respond(res)

	logger.Info("Linphone 注册成功",
		"username", username,
		"device", userCfg.DeviceID,
		"contact", contactURI,
		"expires", expires,
		"ua", userAgent)
}

// sendAuthChallenge 发送 401 认证挑战
func (r *Registrar) sendAuthChallenge(tx sip.ServerTransaction, req *sip.Request) {
	nonce := r.generateNonce()

	r.mu.Lock()
	r.nonces[nonce] = time.Now().Add(60 * time.Second)
	r.mu.Unlock()

	res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
	authValue := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`,
		r.cfg.SIP.Realm, nonce)
	res.AppendHeader(sip.NewHeader("WWW-Authenticate", authValue))
	tx.Respond(res)
}

// validateAuth 验证 Digest 认证
func (r *Registrar) validateAuth(username, authValue string) bool {
	// 解析 Authorization header
	params := r.parseDigestAuth(authValue)

	nonce := params["nonce"]
	response := params["response"]
	uri := params["uri"]

	// 检查 nonce 有效性
	r.mu.Lock()
	nonceTime, ok := r.nonces[nonce]
	if ok {
		delete(r.nonces, nonce)
	}
	r.mu.Unlock()

	if !ok || time.Now().After(nonceTime) {
		logger.RunDebug("Digest 认证: nonce 无效或过期", "username", username, "nonce", nonce)
		return false
	}

	// 查找用户密码
	userCfg := r.findUserConfig(username)
	if userCfg == nil {
		return false
	}

	// 计算预期响应
	// HA1 = MD5(username:realm:password)
	// HA2 = MD5(method:uri)
	// response = MD5(HA1:nonce:HA2)
	ha1 := r.md5sum(username + ":" + r.cfg.SIP.Realm + ":" + userCfg.Password)
	ha2 := r.md5sum("REGISTER:" + uri)
	expected := r.md5sum(ha1 + ":" + nonce + ":" + ha2)

	if response != expected {
		logger.RunDebug("Digest 认证: 响应不匹配",
			"username", username,
			"device", userCfg.DeviceID,
			"expected", expected,
			"got", response)
		return false
	}

	return true
}

// parseDigestAuth 解析 Digest Authorization header
func (r *Registrar) parseDigestAuth(authValue string) map[string]string {
	result := make(map[string]string)

	// 移除 "Digest " 前缀
	authValue = strings.TrimPrefix(authValue, "Digest ")
	authValue = strings.TrimPrefix(authValue, "digest ")

	// 解析 key="value" 或 key=value 对
	parts := strings.Split(authValue, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		idx := strings.Index(part, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		// 移除引号
		value = strings.Trim(value, `"`)
		result[key] = value
	}

	return result
}

// parseExpires 解析 Expires 值
func (r *Registrar) parseExpires(req *sip.Request) int {
	// 优先从 Expires header 获取
	if expiresHeader := req.GetHeader("Expires"); expiresHeader != nil {
		var expires int
		if _, err := fmt.Sscanf(expiresHeader.Value(), "%d", &expires); err == nil {
			return expires
		}
	}

	// 从 Contact header 的 expires 参数获取
	if contact := req.Contact(); contact != nil {
		contactStr := contact.String()
		if idx := strings.Index(strings.ToLower(contactStr), "expires="); idx >= 0 {
			var expires int
			if _, err := fmt.Sscanf(contactStr[idx+8:], "%d", &expires); err == nil {
				return expires
			}
		}
	}

	// 默认 3600 秒
	return 3600
}

// registerUser 注册用户
func (r *Registrar) registerUser(username, deviceID, displayName, contactURI string, contactAddr *net.UDPAddr, transport string, userAgent string, expires int, pushT, pushPv, pushPa, pushCs, pushMs string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	user := &RegisteredUser{
		Username:     username,
		DeviceID:     deviceID,
		DisplayName:  displayName,
		ContactURI:   contactURI,
		ContactAddr:  contactAddr,
		Source:       fmt.Sprint(contactAddr), // 默认用 ContactAddr，后面会被覆盖
		Transport:    transport,               // 使用客户端实际的 Transport (UDP/TCP/TLS)
		UserAgent:    userAgent,
		Expires:      time.Now().Add(time.Duration(expires) * time.Second),
		PushToken:    pushT,
		PushProvider: pushPv,
		PushParam:    pushPa,
		PushCallStr:  pushCs,
		PushMsgStr:   pushMs,
	}

	// 尝试提取更准确的 Source 和 Transport
	// 注意: 这里的 contactAddr 其实是根据 req.Source() 解析的 UDP 地址，
	// 如果实际是 TCP，这里的数据可能不准确，应当在调用处传入准确信息。
	// 但为了兼容现有签名，我们暂时保持这样。

	r.users[username] = user
	if deviceID != "" {
		r.byDevice[deviceID] = user
		if ch, ok := r.onlineSignals[deviceID]; ok && ch != nil {
			close(ch)
		}
		r.onlineSignals[deviceID] = make(chan struct{})
	}
}

// unregisterUser 注销用户
func (r *Registrar) unregisterUser(username string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if user, ok := r.users[username]; ok {
		// 只清除在线连接信息，保留 Push Token 以便离线时推送唤醒
		user.ContactAddr = nil
		user.ContactURI = ""
		user.Source = ""
		user.Transport = ""
		// 不 delete r.users[username] 和 r.byDevice[deviceID]
		// Push 相关字段 (PushToken, PushProvider, PushParam 等) 保留
	}
}

func (r *Registrar) SubscribeDeviceOnline(deviceID string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.onlineSignals[deviceID]
	if !ok || ch == nil {
		ch = make(chan struct{})
		r.onlineSignals[deviceID] = ch
	}
	return ch
}

// findUserConfig 查找用户配置
func (r *Registrar) findUserConfig(username string) *UserConfig {
	for i := range r.cfg.Users {
		if r.cfg.Users[i].Username == username {
			return &r.cfg.Users[i]
		}
	}
	return nil
}

// GetUserByDevice 根据设备 ID 查找已注册用户
func (r *Registrar) GetUserByDevice(deviceID string) *RegisteredUser {
	r.mu.RLock()
	defer r.mu.RUnlock()

	user := r.byDevice[deviceID]
	if user != nil {
		if time.Now().Before(user.Expires) {
			return user
		}
		// 调试日志：用户已过期
		logger.RunDebug("GetUserByDevice: 用户已过期", "device", deviceID, "username", user.Username, "expires", user.Expires)
	} else {
		// 调试日志：未找到用户 (仅在某些情况下记录，避免刷屏？不，现在需要调试)
		// logger.Debug("GetUserByDevice: 未找到用户", "device", deviceID)
	}
	return nil
}

// GetClientContact 获取软电话的联系人信息（适配 ClientAdapter 接口）
func (r *Registrar) GetClientContact(deviceID string) (contactURI string, contactIP string, username string, err error) {
	user := r.GetUserByDevice(deviceID)
	if user == nil {
		return "", "", "", fmt.Errorf("user not found for device: %s", deviceID)
	}

	contactAddr := ""
	if user.ContactAddr != nil {
		contactAddr = user.ContactAddr.String()
	}

	return user.ContactURI, contactAddr, user.Username, nil
}

// GetUserByUsername 根据用户名查找已注册用户
func (r *Registrar) GetUserByUsername(username string) *RegisteredUser {
	r.mu.RLock()
	defer r.mu.RUnlock()

	user := r.users[username]
	if user != nil && time.Now().Before(user.Expires) {
		return user
	}
	return nil
}

// GetAllUsers 获取所有在线用户
func (r *Registrar) GetAllUsers() []*RegisteredUser {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*RegisteredUser
	now := time.Now()
	for _, u := range r.users {
		if now.Before(u.Expires) {
			result = append(result, u)
		}
	}
	return result
}

// cleanupLoop 定期清理过期注册
func (r *Registrar) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.cleanup()
		}
	}
}

// cleanup 清理过期的注册和 nonce
func (r *Registrar) cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// 清理过期用户
	for username, user := range r.users {
		if now.After(user.Expires) {
			logger.RunDebug("清理过期用户注册", "username", username)
			if user.DeviceID != "" {
				delete(r.byDevice, user.DeviceID)
			}
			delete(r.users, username)
		}
	}

	// 清理过期 nonce
	for nonce, expTime := range r.nonces {
		if now.After(expTime) {
			delete(r.nonces, nonce)
		}
	}
}

// respond 发送 SIP 响应
func (r *Registrar) respond(tx sip.ServerTransaction, req *sip.Request, code int, reason string) {
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	if err := tx.Respond(res); err != nil {
		logger.Warn("Registrar 发送 SIP 响应失败", "code", code, "reason", reason, "err", err)
	}
}

// generateNonce 生成认证 nonce
func (r *Registrar) generateNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sendPushDigestAuth 实现 HTTP DIGEST 摘要防重放双阶鉴权（增加推测鉴权 1-RTT 降迟）
func (r *Registrar) sendPushDigestAuth(reqTemplate *http.Request, fromUser string, payloadBytes []byte) (*http.Response, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	username := r.cfg.LinphonePush.LinphoneUser
	password := r.cfg.LinphonePush.LinphonePassword
	uri := reqTemplate.URL.RequestURI()

	// 1. 尝试推测鉴权 (1-RTT 极速推送)
	r.pushAuthMu.Lock()
	realm := r.pushAuthCache["realm"]
	nonce := r.pushAuthCache["nonce"]
	qop := r.pushAuthCache["qop"]
	opaque := r.pushAuthCache["opaque"]
	algorithm := r.pushAuthCache["algorithm"]
	r.pushAuthMu.Unlock()

	if nonce != "" {
		reqP, err := http.NewRequest(reqTemplate.Method, reqTemplate.URL.String(), bytes.NewReader(payloadBytes))
		if err == nil {
			for k, v := range reqTemplate.Header {
				reqP.Header[k] = v
			}
			reqP.Header.Set("from", fmt.Sprintf("sip:%s@sip.linphone.org", fromUser))

			ha1 := r.md5sum(username + ":" + realm + ":" + password)
			ha2 := r.md5sum(reqP.Method + ":" + uri)

			var response string
			var authValue string

			if qop == "auth" || strings.Contains(qop, "auth") {
				qopMatch := "auth"
				r.pushAuthMu.Lock()
				r.pushAuthNC++
				ncVal := r.pushAuthNC
				r.pushAuthMu.Unlock()

				nc := fmt.Sprintf("%08x", ncVal)
				cnonce := fmt.Sprintf("%08x", time.Now().UnixNano())
				response = r.md5sum(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qopMatch + ":" + ha2)
				authValue = fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=%s, nc=%s, cnonce="%s", response="%s", opaque="%s", algorithm=%s`,
					username, realm, nonce, uri, qopMatch, nc, cnonce, response, opaque, algorithm)
			} else {
				response = r.md5sum(ha1 + ":" + nonce + ":" + ha2)
				authValue = fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", opaque="%s", algorithm=%s`,
					username, realm, nonce, uri, response, opaque, algorithm)
			}
			reqP.Header.Set("Authorization", authValue)

			resp, err := client.Do(reqP)
			if err == nil {
				if resp.StatusCode != http.StatusUnauthorized {
					logger.RunDebug("APNs 推测鉴权(1-RTT)命中成功！")
					return resp, nil
				}
				// Http 401，说明 nonce 已过期，将流抛弃后降级进入常规模式
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}

	// 2. 发射探测请求 (要求携带 From: sip:xxx@sip.linphone.org)
	reqProbe, err := http.NewRequest(reqTemplate.Method, reqTemplate.URL.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range reqTemplate.Header {
		reqProbe.Header[k] = v
	}
	reqProbe.Header.Set("from", fmt.Sprintf("sip:%s@sip.linphone.org", fromUser))

	resp, err := client.Do(reqProbe)
	if err != nil {
		return nil, err
	}

	// 如果服务器直接给过（如误判配了免密白名单），直接返回
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// 3. 截获 401 Unauthorized，提取由于防御重放生成的 nonce 和 realm
	authHeader := resp.Header.Get("www-authenticate")
	if authHeader == "" {
		authHeader = resp.Header.Get("Www-Authenticate")
	}

	// 发射探测请求结束后必须释放流
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if authHeader == "" || !strings.HasPrefix(strings.ToLower(authHeader), "digest") {
		return resp, nil // 非 Digest 挑战直接返回原始错让外层输出日志
	}

	challenge := r.parseDigestAuth(authHeader)
	realm = challenge["realm"]
	nonce = challenge["nonce"]
	qop = challenge["qop"]
	opaque = challenge["opaque"]
	algorithm = challenge["algorithm"]
	if algorithm == "" {
		algorithm = "MD5"
	}

	// 存入缓存供下次加速复用
	r.pushAuthMu.Lock()
	if r.pushAuthCache == nil {
		r.pushAuthCache = make(map[string]string)
	}
	r.pushAuthCache["realm"] = realm
	r.pushAuthCache["nonce"] = nonce
	r.pushAuthCache["qop"] = qop
	r.pushAuthCache["opaque"] = opaque
	r.pushAuthCache["algorithm"] = algorithm
	r.pushAuthNC = 0
	r.pushAuthMu.Unlock()

	// 4. 构建真正的带有 MD5 Digest 的第二次 POST 请求
	req2, err := http.NewRequest(reqTemplate.Method, reqTemplate.URL.String(), bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}
	// 继承其它业务 Header (包含 Content-Type 等)
	for k, v := range reqTemplate.Header {
		req2.Header[k] = v
	}
	req2.Header.Set("from", fmt.Sprintf("sip:%s@sip.linphone.org", fromUser))

	// 5. 计算 RFC-7616 哈希摘要
	ha1 := r.md5sum(username + ":" + realm + ":" + password)
	ha2 := r.md5sum(req2.Method + ":" + uri)

	var response string
	var authValue string

	if qop == "auth" || strings.Contains(qop, "auth") {
		qopMatch := "auth"
		r.pushAuthMu.Lock()
		r.pushAuthNC++
		ncVal := r.pushAuthNC
		r.pushAuthMu.Unlock()

		nc := fmt.Sprintf("%08x", ncVal)
		cnonce := fmt.Sprintf("%08x", time.Now().UnixNano())
		response = r.md5sum(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qopMatch + ":" + ha2)
		authValue = fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=%s, nc=%s, cnonce="%s", response="%s", opaque="%s", algorithm=%s`,
			username, realm, nonce, uri, qopMatch, nc, cnonce, response, opaque, algorithm)
	} else {
		response = r.md5sum(ha1 + ":" + nonce + ":" + ha2)
		authValue = fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", opaque="%s", algorithm=%s`,
			username, realm, nonce, uri, response, opaque, algorithm)
	}

	req2.Header.Set("Authorization", authValue)

	// 6. 将验证包射出去
	return client.Do(req2)
}

// SendPushNotification 使用官方PUSH证书发送推送唤醒
func (r *Registrar) SendPushNotification(deviceID string, callID string, caller string, callee string) error {

	user := r.GetUserByDevice(deviceID)
	if user == nil {
		return fmt.Errorf("设备 %s 的会话不存在，无法下发推送", deviceID)
	}
	if user.PushProvider == "" || user.PushToken == "" {
		return fmt.Errorf("该设备没有上报过苹果/安卓推送令牌 (pn-provider=%s, pn-prid=%s)", user.PushProvider, user.PushToken)
	}

	password := r.cfg.LinphonePush.LinphonePassword
	fromUser := r.cfg.LinphonePush.LinphoneUser
	if password == "" || fromUser == "" {
		return fmt.Errorf("未配置 Linphone 推送凭证 (linphone_password 或 linphone_user 为空)，跳过网络唤醒")
	}

	logger.RunDebug("尝试使用官方PUSH证书发送推送唤醒", "device", deviceID, "call_id", callID)

	payload := map[string]interface{}{
		"pn_provider": user.PushProvider,
		"pn_prid":     user.PushToken,
		"pn_param":    user.PushParam,
		"pn_call_str": user.PushCallStr,
		"pn_msg_str":  user.PushMsgStr,
		"loc_args":    caller, // 很多 App 会直接取 loc_args 作为本地化弹窗主叫占位
		"from_uri":    fmt.Sprintf("sip:%s@ims.mnc033.mcc234.3gppnetwork.org", caller),
		"type":        "call",
		"call_id":     callID,
	}

	body, _ := json.Marshal(payload)

	// 在探测请求中不要携带 payload byte reader，避免被单次消费
	req, err := http.NewRequest("POST", "https://subscribe.linphone.org/api/push_notification", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// 使用 HTTP DIGEST 发起鉴权并投递
	resp, err := r.sendPushDigestAuth(req, fromUser, body)
	if err != nil {
		return fmt.Errorf("推送 HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("官方推送网关拒收 (Http %d): %s", resp.StatusCode, string(respBytes))
	}

	logger.Info("推送指令已被 Linphone 官方服务器接收，期待手机CallKit通知！", "device", deviceID, "push_type", user.PushProvider)
	return nil
}

// md5sum 计算 MD5 哈希
func (r *Registrar) md5sum(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// SetOnInvite 设置呼出 INVITE 回调
func (r *Registrar) SetOnInvite(fn func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onInvite = fn
}

// SetOnCancel 设置取消回调
func (r *Registrar) SetOnCancel(fn func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onCancel = fn
}

// SetOnAck 设置 ACK 回调
func (r *Registrar) SetOnAck(f func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onAck = f
}

// SetOnBye 设置 BYE 回调
func (r *Registrar) SetOnBye(f func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onBye = f
}

// handleInvite 处理 Linphone 呼出 INVITE
func (r *Registrar) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	logger.Info("Registrar: 收到 INVITE 请求", "source", req.Source(), "to", req.To().Address.String(), "call_id", req.CallID().Value())

	from := req.From()
	if from == nil {
		r.respond(tx, req, 400, "Bad Request - Missing From")
		return
	}
	username := from.Address.User

	// 检查是否是已注册用户
	user := r.GetUserByUsername(username)
	if user == nil {
		logger.Warn("呼出 INVITE: 用户未注册", "username", username)
		r.respond(tx, req, 403, "Forbidden - Not Registered")
		return
	}

	// 获取被叫号码
	to := req.To()
	if to == nil {
		r.respond(tx, req, 400, "Bad Request - Missing To")
		return
	}
	callee := to.Address.User

	logger.Info("收到 Linphone 呼出 INVITE",
		"from", username,
		"to", callee,
		"device", user.DeviceID)

	// 先发送 100 Trying
	r.respond(tx, req, 100, "Trying")

	// 调用呼出回调
	r.mu.RLock()
	fn := r.onInvite
	r.mu.RUnlock()

	if fn != nil {
		// 并发控制：尝试获取信号量
		select {
		case r.concurrencySem <- struct{}{}:
			// 获取成功，启动异步调用，不阻塞 sipgo 的 goroutine
			go func() {
				defer func() { <-r.concurrencySem }() // 执行完毕不仅释放 Transaction，也释放信号量
				fn(user.DeviceID, req, tx)
			}()
		default:
			// 获取失败（过载），返回 503
			logger.Warn("呼出 INVITE: 服务器繁忙 (并发超限)", "username", username, "device", user.DeviceID)
			r.respond(tx, req, 503, "Service Unavailable")
		}
	} else {
		logger.Warn("呼出 INVITE: 无回调处理器", "username", username, "device", user.DeviceID)
		r.respond(tx, req, 503, "Service Unavailable")
	}
}

// handleAck 处理 ACK
func (r *Registrar) handleAck(req *sip.Request, tx sip.ServerTransaction) {
	// logger.Debug("收到 ACK", "call_id", req.CallID().Value())

	from := req.From()
	if from == nil {
		return
	}
	username := from.Address.User
	user := r.GetUserByUsername(username)
	if user == nil {
		return
	}

	if r.onAck != nil {
		go r.onAck(user.DeviceID, req, tx)
	}
}

// handleBye 处理挂断
func (r *Registrar) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	from := req.From()
	username := ""
	if from != nil {
		username = from.Address.User
	}

	// 打印完整 BYE 消息用于诊断秒断问题
	reasonHeader := req.GetHeader("Reason")
	reason := ""
	if reasonHeader != nil {
		reason = reasonHeader.Value()
	}
	warningHeader := req.GetHeader("Warning")
	warning := ""
	if warningHeader != nil {
		warning = warningHeader.Value()
	}

	if shouldLogSIPRaw() {
		logger.RunDebug("收到 Linphone BYE",
			"from", username,
			"call_id", req.CallID().Value(),
			"reason", reason,
			"warning", warning,
			"source", req.Source(),
			"full_msg", redactSIPRaw(req.String()))
	} else {
		logger.RunDebug("收到 Linphone BYE",
			"from", username,
			"call_id", req.CallID().Value(),
			"reason", reason,
			"warning", warning,
			"source", req.Source())
	}

	// 回复 200 OK 给 Linphone
	r.respond(tx, req, 200, "OK")

	// 通知 Gateway → Agent → 转发 BYE 到 IMS
	user := r.GetUserByUsername(username)
	if r.onBye != nil && user != nil {
		go r.onBye(user.DeviceID, req, tx)
	}
}

// handleCancel 处理 CANCEL 请求
func (r *Registrar) handleCancel(req *sip.Request, tx sip.ServerTransaction) {
	from := req.From()
	if from == nil {
		r.respond(tx, req, 400, "Bad Request - Missing From")
		return
	}
	username := from.Address.User

	logger.RunDebug("收到 Linphone CANCEL", "username", username, "call_id", req.CallID().Value())

	r.mu.RLock()
	onCancel := r.onCancel
	r.mu.RUnlock()

	user := r.GetUserByUsername(username)
	if onCancel != nil && user != nil {
		go onCancel(user.DeviceID, req, tx)
	} else {
		// 如果没有回调，默认发送 200 OK 并尝试终止事务
		r.respond(tx, req, 200, "OK")
	}
}

// SetOnPrack 设置 PRACK 回调
func (r *Registrar) SetOnPrack(handler func(deviceID string, req *sip.Request, tx sip.ServerTransaction)) {
	r.mu.Lock()
	r.onPrack = handler
	r.mu.Unlock()
}

// handlePrack 处理 PRACK 请求
func (r *Registrar) handlePrack(req *sip.Request, tx sip.ServerTransaction) {
	username := extractUsername(req.From())
	// logger.Debug("收到 PRACK 请求", "username", username, "call_id", req.CallID().Value())

	r.mu.RLock()
	handler := r.onPrack
	r.mu.RUnlock()

	user := r.GetUserByUsername(username)
	if handler != nil && user != nil {
		go handler(user.DeviceID, req, tx)
	} else {
		// 默认回复 481 Call/Transaction Does Not Exist
		if err := tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)); err != nil {
			logger.Warn("发送 481 失败", "err", err)
		}
	}
}

func extractUsername(from *sip.FromHeader) string {
	if from == nil {
		return ""
	}
	return from.Address.User
}
