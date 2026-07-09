package vowifihost

import (
	"strings"
	"sync"
	"time"

	"github.com/1239t/vowifi-go/runtimehost"
)

type RuntimeStore interface {
	BeginStart(deviceID string) StartClaim
	ClaimStarted(deviceID string, epoch uint64, inst *runtimehost.Instance) bool
	FailStart(deviceID string, epoch uint64, state runtimehost.State, err error)
	RecordStartupState(deviceID string, state runtimehost.State) bool
	ClearStartupState(deviceID string) bool
	Invalidate(deviceID string) (uint64, bool)
	CurrentEpoch(deviceID string) uint64
	Active(deviceID string) bool
	Starting(deviceID string) bool
	Instance(deviceID string) *runtimehost.Instance
	SetInstance(deviceID string, inst *runtimehost.Instance)
	DeleteInstance(deviceID string, inst *runtimehost.Instance) bool
	State(deviceID string) (runtimehost.State, bool)
	Instances() map[string]*runtimehost.Instance
	InstanceIDs() []string
}

type Store struct {
	mu    sync.RWMutex
	slots map[string]*runtimeSlot
}

type runtimeSlot struct {
	instance  *runtimehost.Instance
	starting  bool
	epoch     uint64
	state     runtimehost.State
	lastErr   string
	updatedAt time.Time
}

type StartClaim struct {
	Epoch    uint64
	Accepted bool
	Active   bool
	Starting bool
}

func NewRuntimeStore() *Store {
	return &Store{slots: make(map[string]*runtimeSlot)}
}

func (s *Store) ensureSlotLocked(deviceID string) *runtimeSlot {
	if s.slots == nil {
		s.slots = make(map[string]*runtimeSlot)
	}
	slot := s.slots[deviceID]
	if slot == nil {
		slot = &runtimeSlot{}
		s.slots[deviceID] = slot
	}
	return slot
}

func (s *Store) BeginStart(deviceID string) StartClaim {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return StartClaim{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.ensureSlotLocked(deviceID)
	if slot.instance != nil {
		return StartClaim{Epoch: slot.epoch, Active: true}
	}
	if slot.starting {
		return StartClaim{Epoch: slot.epoch, Starting: true}
	}
	slot.starting = true
	slot.lastErr = ""
	slot.updatedAt = time.Now()
	return StartClaim{Epoch: slot.epoch, Accepted: true}
}

func (s *Store) ClaimStarted(deviceID string, epoch uint64, inst *runtimehost.Instance) bool {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" || inst == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.ensureSlotLocked(deviceID)
	if slot.epoch != epoch {
		return false
	}
	if slot.instance != nil {
		return false
	}
	slot.instance = inst
	slot.starting = false
	slot.state = runtimehost.State{}
	slot.lastErr = ""
	slot.updatedAt = time.Now()
	return true
}

func (s *Store) FailStart(deviceID string, epoch uint64, state runtimehost.State, err error) {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.ensureSlotLocked(deviceID)
	if slot.epoch != epoch {
		return
	}
	slot.starting = false
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now()
	}
	slot.state = state
	if err != nil {
		slot.lastErr = err.Error()
	}
	slot.updatedAt = time.Now()
}

func (s *Store) RecordStartupState(deviceID string, state runtimehost.State) bool {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return false
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.ensureSlotLocked(deviceID)
	if slot.instance != nil {
		return false
	}
	if !slot.state.UpdatedAt.IsZero() && state.UpdatedAt.Before(slot.state.UpdatedAt) {
		return false
	}
	slot.state = state
	slot.updatedAt = state.UpdatedAt
	return true
}

func (s *Store) ClearStartupState(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.slots[deviceID]
	if slot == nil || slot.state.UpdatedAt.IsZero() {
		return false
	}
	slot.state = runtimehost.State{}
	slot.updatedAt = time.Now()
	if slot.instance == nil && !slot.starting && slot.lastErr == "" {
		delete(s.slots, deviceID)
	}
	return true
}

func (s *Store) Invalidate(deviceID string) (uint64, bool) {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.ensureSlotLocked(deviceID)
	slot.epoch++
	slot.starting = false
	hadState := !slot.state.UpdatedAt.IsZero()
	slot.state = runtimehost.State{}
	slot.lastErr = ""
	slot.updatedAt = time.Now()
	return slot.epoch, hadState
}

func (s *Store) CurrentEpoch(deviceID string) uint64 {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if slot := s.slots[deviceID]; slot != nil {
		return slot.epoch
	}
	return 0
}

func (s *Store) Active(deviceID string) bool {
	return s.Instance(deviceID) != nil
}

func (s *Store) Starting(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if slot := s.slots[deviceID]; slot != nil {
		return slot.starting
	}
	return false
}

func (s *Store) Instance(deviceID string) *runtimehost.Instance {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if slot := s.slots[deviceID]; slot != nil {
		return slot.instance
	}
	return nil
}

func (s *Store) SetInstance(deviceID string, inst *runtimehost.Instance) {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.ensureSlotLocked(deviceID)
	slot.instance = inst
	slot.starting = false
	slot.state = runtimehost.State{}
	slot.lastErr = ""
	slot.updatedAt = time.Now()
}

func (s *Store) DeleteInstance(deviceID string, inst *runtimehost.Instance) bool {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.slots[deviceID]
	if slot == nil || slot.instance == nil {
		return false
	}
	if inst != nil && slot.instance != inst {
		return false
	}
	slot.instance = nil
	slot.starting = false
	slot.state = runtimehost.State{}
	slot.lastErr = ""
	slot.updatedAt = time.Now()
	if slot.epoch == 0 {
		delete(s.slots, deviceID)
	}
	return true
}

func (s *Store) State(deviceID string) (runtimehost.State, bool) {
	deviceID = strings.TrimSpace(deviceID)
	if s == nil || deviceID == "" {
		return runtimehost.State{}, false
	}
	s.mu.RLock()
	slot := s.slots[deviceID]
	if slot == nil {
		s.mu.RUnlock()
		return runtimehost.State{}, false
	}
	inst := slot.instance
	state := slot.state
	hasState := !state.UpdatedAt.IsZero() || state.Phase != "" || slot.starting
	s.mu.RUnlock()
	if inst != nil {
		return inst.State(), true
	}
	if hasState {
		return state, true
	}
	return runtimehost.State{}, false
}

func (s *Store) Instances() map[string]*runtimehost.Instance {
	out := make(map[string]*runtimehost.Instance)
	if s == nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for deviceID, slot := range s.slots {
		if slot != nil && slot.instance != nil {
			out[deviceID] = slot.instance
		}
	}
	return out
}

func (s *Store) InstanceIDs() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.slots))
	for deviceID, slot := range s.slots {
		if slot != nil && slot.instance != nil {
			ids = append(ids, deviceID)
		}
	}
	return ids
}
