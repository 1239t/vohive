package device

import (
	"fmt"

	"github.com/emiago/sipgo/sip"
	"github.com/1239t/vohive/internal/sipgw"
	"github.com/1239t/vowifi-go/runtimehost/voicehost"

	"github.com/1239t/vohive/pkg/logger"
)

// SetVoiceGateway 注入 VoWiFi 语音网关，用于优先走 IMS 外呼/挂断路径。
func (p *Pool) SetVoiceGateway(g *voicehost.Gateway) {
	p.mu.Lock()
	p.voiceGateway = g
	p.mu.Unlock()
	p.voWiFiHost().ConfigureRuntimeDependencies(g, vowifiDeliveryStore{}, poolVoWiFiRuntimeDispatcher{pool: p})
}

// GetVoiceGateway 返回绑定的 VoiceGateway 实例
func (p *Pool) GetVoiceGateway() *voicehost.Gateway {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.voiceGateway
}

// SetSIPRegistrar 注入 SIP 注册器
// 由于此方法通常在 Worker 初始化之后才被调用，
// 需要回扫已有 Worker，给有 AudioDevice 但还没有 CSCallMgr 的 Worker 补创建
func (p *Pool) SetSIPRegistrar(r *sipgw.Registrar) {
	p.mu.Lock()
	p.sipRegistrar = r
	for _, w := range p.workers {
		logger.Debug(fmt.Sprintf("[%s] SetSIPRegistrar 回扫: AudioDevice=%q, CSCallMgr=%v, Modem=%v", w.ID, w.Config.AudioDevice, w.CSCallMgr != nil, w.Modem != nil))
		if w.Config.AudioDevice != "" && w.CSCallMgr == nil {
			w.CSCallMgr = newCSCallManagerForWorker(w, r)
			if w.CSCallMgr != nil {
				logger.Info(fmt.Sprintf("[%s] 已启用 CS 域语音桥接 (AudioDev: %s)", w.ID, w.Config.AudioDevice))
			}
		}
	}
	p.mu.Unlock()

	r.SetOnInvite(func(deviceID string, req *sip.Request, tx sip.ServerTransaction) {
		p.mu.RLock()
		w, ok := p.workers[deviceID]
		voiceGW := p.voiceGateway
		p.mu.RUnlock()

		if voiceGW != nil && voiceGW.GetAgent(deviceID) != nil {
			logger.Info(fmt.Sprintf("[%s] 外呼 INVITE: 优先走 VoWiFi IMS VoiceGateway", deviceID))
			voiceGW.HandleClientInvite(deviceID, req, tx)
			return
		}

		if !ok || w.CSCallMgr == nil {
			logger.Warn(fmt.Sprintf("[%s] 外呼 INVITE: 设备或 CSCall 管理器不存在", deviceID))
			tx.Respond(sip.NewResponseFromRequest(req, 404, "Not Found", nil))
			return
		}
		logger.Info(fmt.Sprintf("[%s] 外呼 INVITE: 回退到 CS 域语音桥接", deviceID))
		w.CSCallMgr.HandleOutboundInvite(deviceID, req, tx)
	})

	r.SetOnBye(func(deviceID string, req *sip.Request, tx sip.ServerTransaction) {
		p.mu.RLock()
		w, ok := p.workers[deviceID]
		voiceGW := p.voiceGateway
		p.mu.RUnlock()
		if ok && w.CSCallMgr != nil {
			w.CSCallMgr.HandleClientBye(req.CallID().Value())
			return
		}
		if voiceGW != nil && voiceGW.GetAgent(deviceID) != nil {
			voiceGW.HandleClientBye(deviceID, req, tx)
		}
	})

	r.SetOnCancel(func(deviceID string, req *sip.Request, tx sip.ServerTransaction) {
		p.mu.RLock()
		w, ok := p.workers[deviceID]
		voiceGW := p.voiceGateway
		p.mu.RUnlock()
		if ok && w.CSCallMgr != nil {
			w.CSCallMgr.HandleClientCancel(req.CallID().Value())
			return
		}
		if voiceGW != nil && voiceGW.GetAgent(deviceID) != nil {
			voiceGW.HandleClientCancel(deviceID, req, tx)
		}
	})
}
