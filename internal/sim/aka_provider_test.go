package sim

import (
	"errors"
	"reflect"
	"testing"
	"time"

	swusim "github.com/1239t/vowifi-go/engine/sim"
)

var _ swusim.AKAProvider = (*ATAKAProvider)(nil)

type akaWithPreferenceProviderStub struct {
	result             swusim.AKAResult
	err                error
	lastPreferenceUsed string
}

func (s *akaWithPreferenceProviderStub) CalculateAKAWithPreference(rand16, autn16 []byte, preference string) (swusim.AKAResult, error) {
	s.lastPreferenceUsed = preference
	return s.result, s.err
}

type akaProviderModemFake struct {
	basicErr         error
	logicalCalls     []string
	logicalAIDs      []string
	logicalClosed    []int
	logicalResponses []string
	openLogicalErr   error
	openErrByAID     map[string]error
	openChannelByAID map[string]int
	executeCalls     []string
	resolvedAID      string
	resolvedAIDByApp map[string]string
	resolveSource    string
	resolveErr       error
}

func (f *akaProviderModemFake) DeviceID() string { return "dev-at" }

func (f *akaProviderModemFake) ExecuteATSilent(cmd string, timeout time.Duration) (string, error) {
	f.executeCalls = append(f.executeCalls, cmd)
	return "", errors.New("ExecuteATSilent should not be used for SIMAuth APDU")
}

func (f *akaProviderModemFake) OpenLogicalChannel(aid string) (int, error) {
	f.logicalAIDs = append(f.logicalAIDs, aid)
	if f.openErrByAID != nil {
		if err := f.openErrByAID[aid]; err != nil {
			return 0, err
		}
	}
	if f.openLogicalErr != nil {
		return 0, f.openLogicalErr
	}
	if f.openChannelByAID != nil {
		if ch := f.openChannelByAID[aid]; ch != 0 {
			return ch, nil
		}
	}
	return 1, nil
}

func (f *akaProviderModemFake) ResolveLogicalChannelAID(app string, fallbackAID string) (string, string, error) {
	if f.resolveErr != nil {
		return "", "", f.resolveErr
	}
	if f.resolvedAIDByApp != nil {
		if aid := f.resolvedAIDByApp[app]; aid != "" {
			return aid, f.resolveSource, nil
		}
	}
	if f.resolvedAID == "" {
		return fallbackAID, "fallback_test", nil
	}
	return f.resolvedAID, f.resolveSource, nil
}
func (f *akaProviderModemFake) CloseLogicalChannel(channel int) error {
	f.logicalClosed = append(f.logicalClosed, channel)
	return nil
}
func (f *akaProviderModemFake) TransmitAPDU(channel int, hexAPDU string) (string, error) {
	f.logicalCalls = append(f.logicalCalls, hexAPDU)
	if len(f.logicalResponses) > 0 {
		resp := f.logicalResponses[0]
		f.logicalResponses = f.logicalResponses[1:]
		return resp, nil
	}
	return "9000", nil
}

func TestATAKAProviderUSIMUsesLogicalChannel(t *testing.T) {
	modem := &akaProviderModemFake{
		resolvedAID:   "A0000000871002FF44FF128900000100",
		resolveSource: "qmi_card_status",
		logicalResponses: []string{
			"DB02112210000102030405060708090A0B0C0D0E0F1000101112131415161718191A1B1C1D1E1F9000",
		},
	}
	provider := NewATAKAProvider(modem)

	res, err := provider.CalculateAKAWithPreference(bytes16(0x10), bytes16(0x20), AKAAppPreferenceUSIM)
	if err != nil {
		t.Fatalf("CalculateAKAWithPreference() error = %v", err)
	}

	if len(res.RES) == 0 || len(res.CK) == 0 || len(res.IK) == 0 {
		t.Fatalf("AKA result missing fields: %+v", res)
	}
	if !reflect.DeepEqual(modem.logicalAIDs, []string{"A0000000871002FF44FF128900000100"}) {
		t.Fatalf("logical AIDs = %#v, want resolved full USIM AID", modem.logicalAIDs)
	}
	if len(modem.logicalCalls) == 0 {
		t.Fatal("expected AKA APDU over logical channel")
	}
	if !reflect.DeepEqual(modem.logicalClosed, []int{1}) {
		t.Fatalf("logicalClosed = %#v, want channel 1 closed", modem.logicalClosed)
	}
}

func TestATAKAProviderUSIMUsesResolvedFullAID(t *testing.T) {
	modem := &akaProviderModemFake{
		resolvedAID:   "A0000000871002FF49FF0189",
		resolveSource: "qmi_card_status",
		logicalResponses: []string{
			"DB02112210000102030405060708090A0B0C0D0E0F1000101112131415161718191A1B1C1D1E1F9000",
		},
	}
	provider := NewATAKAProvider(modem)

	_, err := provider.CalculateAKAWithPreference(bytes16(0x10), bytes16(0x20), AKAAppPreferenceUSIM)
	if err != nil {
		t.Fatalf("CalculateAKAWithPreference() error = %v", err)
	}

	if !reflect.DeepEqual(modem.logicalAIDs, []string{"A0000000871002FF49FF0189"}) {
		t.Fatalf("logical AIDs = %#v, want resolved full USIM AID", modem.logicalAIDs)
	}
}

func TestATAKAProviderUSIMOpenFailureDoesNotTryStaticFallbackAID(t *testing.T) {
	modem := &akaProviderModemFake{
		resolvedAID:   "A0000000871002FF44FF128900000100",
		resolveSource: "qmi_card_status",
		openErrByAID: map[string]error{
			"A0000000871002FF44FF128900000100": errors.New("current aid temporarily rejected"),
		},
	}
	provider := NewATAKAProvider(modem)

	_, err := provider.CalculateAKAWithPreference(bytes16(0x10), bytes16(0x20), AKAAppPreferenceUSIM)
	if err == nil {
		t.Fatal("CalculateAKAWithPreference() err=nil, want open failure")
	}

	wantAIDs := []string{"A0000000871002FF44FF128900000100"}
	if !reflect.DeepEqual(modem.logicalAIDs, wantAIDs) {
		t.Fatalf("logical AIDs = %#v, want %#v", modem.logicalAIDs, wantAIDs)
	}
	if len(modem.logicalClosed) != 0 {
		t.Fatalf("logicalClosed = %#v, want no closed channel", modem.logicalClosed)
	}
}

func TestATAKAProviderISIMStrictDoesNotFallbackToUSIM(t *testing.T) {
	modem := &akaProviderModemFake{
		resolvedAID:    "A0000000871004FFFFFFFF8903020000",
		resolveSource:  "qmi_card_status",
		openLogicalErr: errors.New("isim open failed"),
	}
	provider := NewATAKAProvider(modem)

	_, err := provider.CalculateAKAWithPreference(bytes16(0x10), bytes16(0x20), "isim_strict")
	if err == nil {
		t.Fatal("CalculateAKAWithPreference() err=nil, want strict ISIM failure")
	}

	if len(modem.logicalCalls) != 0 {
		t.Fatalf("logicalCalls = %#v, want no APDU after open failure", modem.logicalCalls)
	}
	if !reflect.DeepEqual(modem.logicalAIDs, []string{"A0000000871004FFFFFFFF8903020000"}) {
		t.Fatalf("logical AIDs = %#v, want full ISIM AID only", modem.logicalAIDs)
	}
}

func TestWrapPreferredAKAProviderReturnsSWUAKAProvider(t *testing.T) {
	stub := &akaWithPreferenceProviderStub{
		result: swusim.AKAResult{
			RES:  []byte{0x01, 0x02},
			CK:   []byte{0x03, 0x04},
			IK:   []byte{0x05, 0x06},
			AUTS: []byte{0x07, 0x08},
		},
	}

	wrapped := WrapPreferredAKAProvider(stub, AKAAppPreferenceISIMStrict)
	var provider swusim.AKAProvider = wrapped

	got, err := provider.CalculateAKA(bytes16(0x10), bytes16(0x20))
	if err != nil {
		t.Fatalf("CalculateAKA() error = %v", err)
	}
	if stub.lastPreferenceUsed != AKAAppPreferenceISIMStrict {
		t.Fatalf("lastPreferenceUsed = %q, want %q", stub.lastPreferenceUsed, AKAAppPreferenceISIMStrict)
	}
	if !reflect.DeepEqual(got, stub.result) {
		t.Fatalf("CalculateAKA() = %+v, want %+v", got, stub.result)
	}
}

func bytes16(start byte) []byte {
	out := make([]byte, 16)
	for i := range out {
		out[i] = start + byte(i)
	}
	return out
}
