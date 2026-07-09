package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/1239t/vohive/internal/api"
	"github.com/1239t/vohive/internal/config"
	"github.com/1239t/vohive/internal/db"
	"github.com/1239t/vohive/internal/device"
	"github.com/1239t/vohive/internal/notify"
	proxyserver "github.com/1239t/vohive/internal/proxy/server"
	"github.com/1239t/vohive/internal/proxy/traffic"
	"github.com/1239t/vohive/internal/sipgw"
	"github.com/1239t/vohive/internal/upstreamproxy"
	"github.com/1239t/vowifi-go/runtimehost/carrier"
	"github.com/1239t/vowifi-go/runtimehost/voicehost"

	"github.com/1239t/vohive/internal/web"
	"github.com/1239t/vohive/pkg/logger"

	"github.com/emiago/sipgo/sip"
)

func migrateLegacyServerDB(legacyPath, targetPath string) error {
	legacyPath = filepath.Clean(legacyPath)
	targetPath = filepath.Clean(targetPath)
	if legacyPath == targetPath {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	if _, err := os.Stat(targetPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat target db: %w", err)
	}
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat legacy db: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from := legacyPath + suffix
		to := targetPath + suffix
		if _, err := os.Stat(from); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("stat legacy db sidecar %s: %w", suffix, err)
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("rename legacy db sidecar %s: %w", suffix, err)
		}
	}
	return nil
}

func main() {
	// 开启 SIP_DEBUG 以排查问题（针对旧系统或备用系统）
	os.Setenv("SIP_DEBUG", "false")

	sipLogger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	sip.SetDefaultLogger(sipLogger)
	sip.SIPDebug = false
	// 绕过 sipgo 底层硬编码的 UDP MTU 限制（默认 1500），
	// 防止由于包含 APNs/FCM 推送 Token 的超长 Contact URI 导致 UDP 发送直接报错。
	// 大包会自动在 IP 层被切片(IP Fragmentation)。
	sip.UDPMTUSize = 65535
	// Parse flags
	var configPath string
	var backendOnly bool
	flag.StringVar(&configPath, "c", "config/config.yaml", "config file path")
	flag.BoolVar(&backendOnly, "backend-only", false, "run as backend-only (disable embedded web UI)")
	flag.Parse()

	// 1. 加载配置
	if err := config.InitGlobalManager(configPath); err != nil {
		log.Fatalf("初始化配置管理器失败: %v", err)
	}
	cfg := config.GetConfig()

	// 2. 初始化日志
	logger.Setup(logger.LogConfig{
		Debug:    cfg.Server.Debug,
		Filename: "logs/app.log",
	})
	// 将内置 slog 重定向到已就绪的系统日志框架
	slog.SetDefault(slog.New(logger.NewSlogHandler(logger.ZapLogger())))
	logger.Info("VoHive 模组管理器启动中...")

	go func() {
		disclaimer := `
======================================================================
【VoHive 免责与使用声明】
1. 本软件仅供个人技术测试与研究交流，严禁任何商业用途。
2. 严禁将本软件用于任何非法或违规场景。
3. 本软件涉及底层通信操作，因测试产生的硬件、资费或网络风险由用户自行承担。
4. 作者不对使用本软件造成的任何直接或间接损失负责。
======================================================================`
		logger.Warn(disclaimer)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			logger.Warn(disclaimer)
		}
	}()

	loadResult, err := carrier.LoadCarrierOverrides("data/carrier_overrides.json")
	if err != nil {
		carrier.ClearCarrierOverrides()
		logger.Warn("加载 carrier_overrides 失败，回退内置运营商配置",
			"path", loadResult.Path,
			"err", err)
	} else if loadResult.Missing {
		//logger.Info("carrier_overrides 文件不存在，使用内置运营商配置", "path", loadResult.Path)
	} else {
		logger.Info("carrier_overrides 已加载", "path", loadResult.Path, "entries", loadResult.Count)
	}

	// 3. 初始化数据库
	legacyDBPath := "data/ec20.db"
	dbPath := "data/vohive.db"
	if err := migrateLegacyServerDB(legacyDBPath, dbPath); err != nil {
		log.Fatalf("迁移旧数据库失败: %v", err)
	}
	if err := db.Init(dbPath); err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	dbResolvedPath := dbPath
	if absPath, err := filepath.Abs(dbPath); err == nil {
		dbResolvedPath = absPath
	}
	logger.Info("数据库已初始化", "path", dbPath, "resolved_path", dbResolvedPath)
	countryResult := upstreamproxy.InitCountryTable(context.Background(), upstreamproxy.CountryTableOptions{
		CachePath: upstreamproxy.DefaultCountryTableCachePath,
	})
	if countryResult.Err != nil {
		logger.Warn("MCC/MNC 国家表不可用，VoWiFi 国家代理规则将按未知国家直连",
			"path", countryResult.CachePath,
			"source_url", countryResult.SourceURL,
			"source", countryResult.Source,
			"err", countryResult.Err)
	} else {
		logger.Info("MCC/MNC 国家表已加载",
			"path", countryResult.CachePath,
			"source", countryResult.Source,
			"rows", countryResult.RowCount,
			"countries", countryResult.Countries)
	}
	go func() {
		need, err := db.NeedBackfillSMSContacts()
		if err != nil {
			logger.Error("短信联系人回填检查失败", "err", err)
			return
		}
		if !need {
			return
		}
		logger.Info("开始短信联系人回填")
		if err := db.BackfillSMSPeerAndContacts(1000); err != nil {
			logger.Error("短信联系人回填失败", "err", err)
			return
		}
		logger.Info("短信联系人回填完成")
	}()

	// 4. 初始化设备池

	pool := device.NewPool(cfg)

	// 卡策略：注入 db-backed resolver；一次性把旧 yaml 策略种子进 card_policies。
	pool.SetPolicyResolver(db.CardPolicyResolver{})

	legacy, err := config.ReadLegacyDevicePoliciesFromYAML(configPath, func(deviceID string) string {
		// device id → 当前 IMEI → ICCID（查不到返回空串使该条跳过）
		return db.CurrentICCIDForDevice(deviceID)
	})
	if err == nil {
		var count int64
		db.DB.Model(&db.CardPolicy{}).Count(&count)
		if count == 0 { // 仅当 policy 表为空时迁移
			n, _ := config.SeedLegacyDevicePolicies(legacy, func(iccid string, p config.LegacyDevicePolicy) error {
				policy := db.DefaultCardPolicy(iccid)
				policy.NetworkEnabled = p.NetworkEnabled
				policy.VoWiFiEnabled = p.VoWiFiEnabled
				policy.IPVersion = p.IPVersion
				policy.APN = p.APN
				return db.UpsertCardPolicy(policy)
			})
			logger.Info("卡策略种子迁移完成", "count", n)
		}
	} else {
		logger.Warn("读取旧 yaml 策略失败，跳过种子迁移", "err", err)
	}

	// 6. 初始化代理实例管理器
	proxyMgr := proxyserver.NewManager()
	logger.Info("代理实例管理器已初始化")

	// 7. 初始化语音网关与软电话 Registrar
	// voiceGW 始终创建，用于管理 VoWiFi Agent（SimulateCall 等）。
	// SIP Registrar（Linphone 软电话接入）仅在 voice_gateway.sip.listen 非空时启用。
	var sipRegistrar *sipgw.Registrar
	var notifyMgr *notify.Manager
	voiceGW := voicehost.NewGateway()
	pool.SetVoiceGateway(voiceGW)

	if err := voiceGW.Start(context.Background()); err != nil {
		logger.Error("语音网关启动失败", "err", err)
	} else {
		logger.Info("语音网关已启动")

		// SIP Registrar（Linphone 软电话）：仅在配置了 sip.listen 时启动
		if cfg.VoWiFi.VoiceGateway.SIP.Listen != "" {
			sipgwCfg := sipgw.Config{
				Enabled: true,
				SIP: sipgw.SIPConfig{
					Listen:     cfg.VoWiFi.VoiceGateway.SIP.Listen,
					Transport:  cfg.VoWiFi.VoiceGateway.SIP.Transport,
					Realm:      cfg.VoWiFi.VoiceGateway.SIP.Realm,
					ExternalIP: cfg.VoWiFi.VoiceGateway.SIP.ExternalIP,
				},
				Media: sipgw.MediaConfig{
					RTPPortMin: cfg.VoWiFi.VoiceGateway.Media.RTPPortMin,
					RTPPortMax: cfg.VoWiFi.VoiceGateway.Media.RTPPortMax,
					Codecs:     cfg.VoWiFi.VoiceGateway.Media.Codecs,
				},
				LinphonePush: sipgw.LinphonePushConfig{
					LinphoneUser:     cfg.VoWiFi.VoiceGateway.LinphonePush.LinphoneUser,
					LinphonePassword: cfg.VoWiFi.VoiceGateway.LinphonePush.LinphonePassword,
				},
			}
			for _, u := range cfg.VoWiFi.VoiceGateway.Users {
				sipgwCfg.Users = append(sipgwCfg.Users, sipgw.UserConfig{
					Username:    u.Username,
					Password:    u.Password,
					DisplayName: u.DisplayName,
					DeviceID:    u.DeviceID,
				})
			}
			if sipgwCfg.SIP.Transport == "" {
				sipgwCfg.SIP.Transport = "udp"
			}
			if sipgwCfg.SIP.Realm == "" {
				sipgwCfg.SIP.Realm = "vohive.local"
			}
			if sipgwCfg.Media.RTPPortMin == 0 {
				sipgwCfg.Media.RTPPortMin = 10000
			}
			if sipgwCfg.Media.RTPPortMax == 0 {
				sipgwCfg.Media.RTPPortMax = 20000
			}

			var err error
			sipRegistrar, err = sipgw.NewRegistrar(sipgwCfg)
			if err != nil {
				logger.Error("Registrar 初始化失败", "err", err)
			} else {
				voiceGW.SetClientAdapter(sipRegistrar)

				sipRegistrar.SetOnInvite(voiceGW.HandleClientInvite)
				sipRegistrar.SetOnCancel(func(deviceID string, req *sip.Request, tx sip.ServerTransaction) {
					callID := req.CallID().Value()
					if w := pool.GetWorker(deviceID); w != nil && w.CSCallMgr != nil && w.CSCallMgr.HasCall(callID) {
						w.CSCallMgr.HandleClientCancel(callID)
						tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
						return
					}
					voiceGW.HandleClientCancel(deviceID, req, tx)
				})
				sipRegistrar.SetOnPrack(voiceGW.HandleClientPrack)
				sipRegistrar.SetOnAck(voiceGW.HandleClientAck)
				sipRegistrar.SetOnBye(func(deviceID string, req *sip.Request, tx sip.ServerTransaction) {
					callID := req.CallID().Value()
					if w := pool.GetWorker(deviceID); w != nil && w.CSCallMgr != nil && w.CSCallMgr.HasCall(callID) {
						w.CSCallMgr.HandleClientBye(callID)
						return
					}
					voiceGW.HandleClientBye(deviceID, req, tx)
				})

				pool.SetSIPRegistrar(sipRegistrar)

				if err := sipRegistrar.Start(context.Background()); err != nil {
					logger.Error("Registrar 启动失败", "err", err)
				} else {
					logger.Info("软电话 Registrar 已启动", "listen", sipgwCfg.SIP.Listen, "users", len(sipgwCfg.Users))
				}
			}
		}

		// 通知管理器初始化
		var err error
		notifyMgr, err = notify.NewManager(cfg, pool)
		if err != nil {
			logger.Warn("通知管理器初始化异常", "err", err)
		} else {
			pool.SetNotifier(notifyMgr)
			voiceGW.SetNotifier(notifyMgr)
		}

	}

	// 5. 启动工作器 (代理, 短信, 健康检查)
	_ = pool.StartAll()

	trafficSampler := traffic.New(traffic.Options{Pool: pool, Mgr: proxyMgr})
	trafficSampler.Start()
	realtimeTraffic := traffic.NewRealtimeManager(traffic.RealtimeOptions{Pool: pool})

	// 7. 启动 API 服务器
	// 准备静态文件系统
	var staticFS http.FileSystem
	if backendOnly {
		logger.Info("启用纯后端模式（未挂载前端静态资源）")
	} else {
		distFS, err := web.GetFS()
		if err != nil {
			log.Fatalf("无法加载嵌入的 Web 文件: %v", err)
		}
		staticFS = http.FS(distFS)
	}

	// 通知管理器如果在上方未初始化，在这里兜底（防止 VoiceGW 未配置/启动的场景）
	if notifyMgr == nil {
		var err error
		notifyMgr, err = notify.NewManager(cfg, pool)
		if err != nil {
			logger.Warn("通知管理器初始化异常", "err", err)
		} else {
			pool.SetNotifier(notifyMgr)
			if voiceGW != nil {
				voiceGW.SetNotifier(notifyMgr)
			}
		}
	}

	apiServer := api.New(cfg, pool, staticFS, proxyMgr, voiceGW, notifyMgr, configPath)
	apiServer.SetRealtimeTraffic(realtimeTraffic)

	syncProxyConfigs := func(reason, deviceID string) {
		if err := apiServer.SyncProxyConfigs(); err != nil {
			logger.Warn("同步代理配置失败", "reason", reason, "device_id", deviceID, "err", err)
			return
		}
		logger.Debug("代理配置已同步", "reason", reason, "device_id", deviceID)
	}
	pool.OnDataConnected(func(deviceID string) {
		syncProxyConfigs("qmi_data_connected", deviceID)
	})

	// 启动后同步代理配置到实例管理器
	go func() {
		time.Sleep(500 * time.Millisecond) // 等待 API 服务器初始化
		syncProxyConfigs("startup", "")
	}()

	apiErrCh := make(chan error, 1)
	go func() {
		if err := apiServer.Run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			apiErrCh <- err
		}
	}()

	logger.Info("所有服务已启动")

	quit := make(chan os.Signal, 2)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	// 8. 等待关闭信号
	var sig os.Signal
	select {
	case sig = <-quit:
		logger.Info("收到关闭信号", "signal", sig.String())
	case err := <-apiErrCh:
		logger.Error("API 服务器失败", "err", err)
	}
	logger.Info("正在优雅关闭所有服务...")

	// 9. 优雅关闭
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	done := make(chan struct{})
	go func() {
		if err := apiServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("关闭 API 服务器时出错", "err", err)
		}

		if notifyMgr != nil {
			notifyMgr.Close()
		}

		trafficSampler.Stop()

		if err := proxyMgr.Shutdown(shutdownCtx); err != nil {
			logger.Error("关闭代理实例时出错", "err", err)
		}

		// 关闭语音网关与软电话 Registrar
		if voiceGW != nil {
			if err := voiceGW.Stop(); err != nil {
				logger.Error("关闭语音网关时出错", "err", err)
			}
		}
		if sipRegistrar != nil {
			if err := sipRegistrar.Stop(); err != nil {
				logger.Error("关闭 Registrar 时出错", "err", err)
			}
		}

		if err := pool.Shutdown(); err != nil {
			logger.Error("关闭工作器池时出错", "err", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-quit:
	case <-time.After(12 * time.Second):
		logger.Warn("关闭超时，强制退出")
	}

	logger.Info("再见!")
}
