package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/gin-gonic/gin"
	"github.com/1239t/vohive/internal/config"
	"github.com/1239t/vohive/internal/db"
	"github.com/1239t/vohive/internal/device"
)

// injectWorker 通过 unsafe 反射将 worker 注入到 pool 的内部 workers map，
// 用于无需完整启动流程的测试场景。
func injectWorker(p *device.Pool, w *device.Worker) {
	pv := reflect.ValueOf(p).Elem().FieldByName("workers")
	m := reflect.NewAt(pv.Type(), unsafe.Pointer(pv.UnsafeAddr())).Elem()
	m.SetMapIndex(reflect.ValueOf(w.ID), reflect.ValueOf(w))
}

func openTestDB(t *testing.T) {
	t.Helper()
	if err := db.Init(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("Init() error=%v", err)
	}
	t.Cleanup(func() {
		if db.DB != nil {
			if sqlDB, err := db.DB.DB(); err == nil && sqlDB != nil {
				_ = sqlDB.Close()
			}
		}
	})
}

func TestGetCardPolicyEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	_ = db.UpsertCardPolicy(db.CardPolicy{ICCID: "8986004", NetworkEnabled: true, IPVersion: "v4", Source: "user"})

	s := &Server{}
	r := gin.Default()
	r.GET("/api/cards/:iccid/policy", s.handleGetCardPolicy)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cards/8986004/policy", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var got db.CardPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.NetworkEnabled {
		t.Fatalf("payload 错: %+v", got)
	}
}

func TestPutCardPolicyEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)
	s := &Server{
		pool: device.NewPool(&config.Config{}),
	}
	r := gin.Default()
	r.PUT("/api/cards/:iccid/policy", s.handlePutCardPolicy)

	body := `{"network_enabled":true,"vowifi_enabled":true,"ip_version":"v4v6","apn":"ims"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/cards/8986005/policy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, _ := db.GetCardPolicy("8986005")
	if !got.NetworkEnabled || !got.VoWiFiEnabled || got.IPVersion != "v4v6" || got.APN != "ims" {
		t.Fatalf("未成功更新: %+v", got)
	}
}

// TestPatchCardPolicyForDevice 验证 patchCardPolicyForDevice helper 正确解析 ICCID 并落库。
func TestPatchCardPolicyForDevice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-patch"}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986patch001")
	injectWorker(p, w)

	s := &Server{pool: p}
	iccid, applied, err := s.patchCardPolicyForDevice("wwan-patch", func(pol *db.CardPolicy) {
		pol.NetworkEnabled = true
		pol.IPVersion = "v4v6"
		pol.APN = "ims"
	})

	if err != nil {
		t.Fatalf("error=%v", err)
	}
	if !applied {
		t.Fatalf("expected applied=true")
	}
	if iccid != "8986patch001" {
		t.Fatalf("iccid=%q", iccid)
	}
	got, err := db.GetCardPolicy("8986patch001")
	if err != nil {
		t.Fatal(err)
	}
	if !got.NetworkEnabled || got.IPVersion != "v4v6" || got.APN != "ims" {
		t.Fatalf("card policy mismatch: %+v", got)
	}
	if got.Source != "user" {
		t.Fatalf("source=%q want user", got.Source)
	}
}

// TestPatchCardPolicyForDeviceNoICCID 验证设备无 ICCID 时 applied=false 且不报错。
func TestPatchCardPolicyForDeviceNoICCID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-nocard"}
	// 不设置 ICCID，模拟无卡状态
	injectWorker(p, w)

	s := &Server{pool: p}
	iccid, applied, err := s.patchCardPolicyForDevice("wwan-nocard", func(pol *db.CardPolicy) {
		pol.NetworkEnabled = true
	})

	if err != nil {
		t.Fatalf("error=%v", err)
	}
	if applied {
		t.Fatalf("expected applied=false when no ICCID")
	}
	if iccid != "" {
		t.Fatalf("iccid=%q want empty", iccid)
	}
}

// TestPatchCardPolicyVoWiFiKeepsAirplaneIntent 验证开 VoWiFi 不再强制 airplane=true：
// airplane 反映用户的纯飞行意图，独立于 vowifi。
func TestPatchCardPolicyVoWiFiKeepsAirplaneIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-vowifi"}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986vowifi01")
	injectWorker(p, w)

	s := &Server{pool: p}
	// 从在线开 VoWiFi（飞行意图为 false）：airplane 应保持 false，不被强制为 true。
	_, _, err := s.patchCardPolicyForDevice("wwan-vowifi", vowifiEnablePolicyMutation)
	if err != nil {
		t.Fatalf("error=%v", err)
	}
	got, _ := db.GetCardPolicy("8986vowifi01")
	if !got.VoWiFiEnabled || got.AirplaneEnabled {
		t.Fatalf("开 VoWiFi 不应强制 airplane=true: vowifi=%v airplane=%v", got.VoWiFiEnabled, got.AirplaneEnabled)
	}
}

// TestVoWiFiToggleCyclePreservesAirplaneIntent 复现并锁定 bug 修复：
// 先开飞行 → 开 VoWiFi → 关 VoWiFi，应回退到飞行（airplane 意图被保留）。
func TestVoWiFiToggleCyclePreservesAirplaneIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-cycle"}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986cycle001")
	injectWorker(p, w)
	s := &Server{pool: p}

	// 1) 用户先开飞行
	if _, _, err := s.patchCardPolicyForDevice("wwan-cycle", func(pol *db.CardPolicy) {
		pol.AirplaneEnabled = true
		pol.VoWiFiEnabled = false
		pol.NetworkEnabled = false
	}); err != nil {
		t.Fatalf("开飞行 error=%v", err)
	}

	// 2) 开 VoWiFi（落库副作用：只置 vowifi）
	if _, _, err := s.patchCardPolicyForDevice("wwan-cycle", vowifiEnablePolicyMutation); err != nil {
		t.Fatalf("开 vowifi error=%v", err)
	}
	mid, _ := db.GetCardPolicy("8986cycle001")
	if !mid.VoWiFiEnabled || !mid.AirplaneEnabled {
		t.Fatalf("开 VoWiFi 期间飞行意图应保留: %+v", mid)
	}

	// 3) 关 VoWiFi（落库副作用：只清 vowifi），应回退到飞行
	if _, _, err := s.patchCardPolicyForDevice("wwan-cycle", vowifiDisablePolicyMutation); err != nil {
		t.Fatalf("关 vowifi error=%v", err)
	}
	got, _ := db.GetCardPolicy("8986cycle001")
	if got.VoWiFiEnabled || !got.AirplaneEnabled {
		t.Fatalf("关 VoWiFi 后应回退到飞行模式: vowifi=%v airplane=%v", got.VoWiFiEnabled, got.AirplaneEnabled)
	}
}

// TestPatchCardPolicyAirplaneMutualExclusion 验证“开飞行模式”落库时与 network/vowifi 互斥
// （等价于 handleDeviceMgmtSetFlightMode 开飞行时的落库副作用）。
func TestPatchCardPolicyAirplaneMutualExclusion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openTestDB(t)

	// 预置：network 开着、vowifi 开着
	_ = db.UpsertCardPolicy(db.CardPolicy{ICCID: "8986air001", NetworkEnabled: true, VoWiFiEnabled: true, Source: "user"})

	p := device.NewPool(&config.Config{})
	w := &device.Worker{ID: "wwan-air"}
	setNestedPrivateField(t, w, []string{"state", "Identity", "ICCID"}, "8986air001")
	injectWorker(p, w)

	s := &Server{pool: p}
	// 开飞行：airplane=on，且互斥关 network/vowifi
	_, applied, err := s.patchCardPolicyForDevice("wwan-air", func(pol *db.CardPolicy) {
		pol.AirplaneEnabled = true
		pol.VoWiFiEnabled = false
		pol.NetworkEnabled = false
	})
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}

	got, _ := db.GetCardPolicy("8986air001")
	if !got.AirplaneEnabled || got.NetworkEnabled || got.VoWiFiEnabled {
		t.Fatalf("开飞行应互斥关 network/vowifi: %+v", got)
	}
}
