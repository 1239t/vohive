package vowifihost

import (
	"testing"

	swusim "github.com/1239t/vowifi-go/engine/sim"
	"github.com/1239t/vowifi-go/runtimehost"
)

var _ swusim.AKAProvider = missingSIMProvider{}

func TestBuildVoWiFiSIMAdapterPrefersOverride(t *testing.T) {
	override := runtimehost.NewReaderSIMAdapter(missingSIMProvider{})
	got := buildVoWiFiSIMAdapter(override, nil, "222")
	if got == nil {
		t.Fatal("override 应被返回")
	}
	fallback := buildVoWiFiSIMAdapter(nil, nil, "333")
	if fallback == nil {
		t.Fatal("回退适配器不应为 nil")
	}
}
