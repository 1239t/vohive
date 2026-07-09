package device

import (
	"context"
	"os"
	"testing"

	"github.com/1239t/vohive/internal/backend"
	"github.com/1239t/vowifi-go/runtimehost/carrier"
)

func TestUTRANCellIDSuffixFromServingSystem(t *testing.T) {
	got := utranCellIDSuffixFromServingSystem(&backend.ServingSystem{
		LAC:    "1A2B",
		CellID: "254AF11",
	})
	want := "1A2B254AF11"
	if got != want {
		t.Fatalf("utranCellIDSuffixFromServingSystem() = %q, want %q", got, want)
	}
}

func TestResolveVoWiFiIMSUTRANCellIDFallsBackToCachedState(t *testing.T) {
	w := &Worker{
		state: deviceStateStore{
			Runtime: deviceRuntimeState{
				LAC:    "00F1",
				CellID: "00ABCDEF",
			},
		},
	}
	got, source := resolveVoWiFiIMSUTRANCellID(context.Background(), w, "234", "10")
	want := "00F10ABCDEF"
	if got != want || source != "qmi" {
		t.Fatalf("resolveVoWiFiIMSUTRANCellID() = (%q,%q), want (%q,qmi)", got, source, want)
	}
}

func TestResolveVoWiFiIMSUTRANCellIDFallsBackToCarrierDefault(t *testing.T) {
	got, source := resolveVoWiFiIMSUTRANCellID(context.Background(), &Worker{}, "234", "10")
	want := "70010BC614E"
	if got != want || source != "carrier_default" {
		t.Fatalf("resolveVoWiFiIMSUTRANCellID() = (%q,%q), want (%q,carrier_default)", got, source, want)
	}
}

func TestResolveVoWiFiIMSUTRANCellIDCarrierOnlySkipsQMI(t *testing.T) {
	t.Cleanup(carrier.ClearCarrierOverrides)
	path := t.TempDir() + "/carrier_overrides.json"
	if err := os.WriteFile(path, []byte(`[{"mcc":"234","mnc":"10","ims_tac":28673,"ims_cell_id":12345678,"ims_cell_id_mode":"carrier_only"}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	if _, err := carrier.LoadCarrierOverrides(path); err != nil {
		t.Fatalf("LoadCarrierOverrides: %v", err)
	}

	w := &Worker{
		state: deviceStateStore{
			Runtime: deviceRuntimeState{
				LAC:    "00F1",
				CellID: "00ABCDEF",
			},
		},
	}
	got, source := resolveVoWiFiIMSUTRANCellID(context.Background(), w, "234", "10")
	want := "70010BC614E"
	if got != want || source != "carrier_default" {
		t.Fatalf("resolveVoWiFiIMSUTRANCellID() = (%q,%q), want (%q,carrier_default)", got, source, want)
	}
}