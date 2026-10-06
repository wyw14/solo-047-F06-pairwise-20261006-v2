package repair

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-103/internal/feed"
	"github.com/wyw14/cry-103/internal/landing"
	"github.com/wyw14/cry-103/internal/platform"
)

type PermitState string

const (
	PermitRequested  PermitState = "requested"
	PermitIsolating  PermitState = "isolating"
	PermitGroundable PermitState = "groundable"
	PermitGrounded   PermitState = "grounded"
	PermitReleased   PermitState = "released"
)

type ExpiryStatus string

const (
	ExpiryActive   ExpiryStatus = "active"
	ExpiryExpiring ExpiryStatus = "expiring"
	ExpiryExpired  ExpiryStatus = "expired"
)

type Permit struct {
	ID          string      `json:"id"`
	StationID   string      `json:"station_id"`
	CircuitID   string      `json:"circuit_id"`
	OperationID string      `json:"operation_id"`
	State       PermitState `json:"state"`
	Generation  uint64      `json:"generation"`
	ValidFrom   time.Time   `json:"valid_from"`
	ExpiresAt   time.Time   `json:"expires_at"`
	RemindAt    time.Time   `json:"remind_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

type PermitVersion struct {
	Version     uint64      `json:"version"`
	PermitID    string      `json:"permit_id"`
	State       PermitState `json:"state"`
	OperationID string      `json:"operation_id"`
	Generation  uint64      `json:"generation"`
	ValidFrom   time.Time   `json:"valid_from"`
	ExpiresAt   time.Time   `json:"expires_at"`
	Reason      string      `json:"reason"`
	RecordedAt  time.Time   `json:"recorded_at"`
}

type PermitView struct {
	Permit
	Expiry           ExpiryStatus `json:"expiry"`
	ExpiresInSeconds int64        `json:"expires_in_seconds"`
}

type PermitService struct {
	mu           sync.RWMutex
	permits      map[string]Permit
	versions     map[string][]PermitVersion
	circuits     map[string]*feed.Circuit
	isolation    *feed.IsolationService
	receipts     *landing.ReceiptRegistry
	ids          platform.IDGenerator
	now          func() time.Time
	ttl          time.Duration
	remindBefore time.Duration
}

func NewPermitService(isolation *feed.IsolationService, receipts *landing.ReceiptRegistry, ids platform.IDGenerator, now func() time.Time, ttl, remindBefore time.Duration) *PermitService {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if remindBefore < 0 || remindBefore > ttl {
		remindBefore = ttl
	}
	return &PermitService{
		permits: make(map[string]Permit), versions: make(map[string][]PermitVersion),
		circuits: make(map[string]*feed.Circuit), isolation: isolation,
		receipts: receipts, ids: ids, now: now, ttl: ttl, remindBefore: remindBefore,
	}
}

func circuitKey(stationID, circuitID string) string {
	return stationID + "\x00" + circuitID
}

func (s *PermitService) RegisterCircuit(circuit *feed.Circuit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := circuit.Snapshot()
	s.circuits[circuitKey(snapshot.StationID, snapshot.ID)] = circuit
}

func (s *PermitService) Request(stationID, circuitID string) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	circuit := s.circuits[circuitKey(stationID, circuitID)]
	if circuit == nil {
		return Permit{}, fmt.Errorf("feed circuit %s/%s not found", stationID, circuitID)
	}
	permit := Permit{
		ID: s.ids.New("permit"), StationID: stationID, CircuitID: circuitID,
		OperationID: s.ids.New("isolation"), State: PermitRequested, Generation: 1,
		UpdatedAt: s.now().UTC(),
	}
	permit.ValidFrom, permit.ExpiresAt, permit.RemindAt = s.validityWindow(s.now())
	if _, err := s.isolation.Begin(circuit, permit.OperationID, s.now()); err != nil {
		return Permit{}, err
	}
	permit.State = PermitIsolating
	s.permits[permit.ID] = permit
	s.recordVersion(permit, "requested")
	return permit, nil
}

func (s *PermitService) Evaluate(permitID string) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return Permit{}, fmt.Errorf("permit %s not found", permitID)
	}
	circuit := s.circuits[circuitKey(permit.StationID, permit.CircuitID)]
	_, matched := s.receipts.Match(permit.StationID, permit.CircuitID, permit.OperationID)
	if _, err := s.isolation.Confirm(circuit, permit.StationID, permit.CircuitID, permit.OperationID, matched, s.now()); err != nil {
		return permit, err
	}
	permit.State = PermitGroundable
	permit.UpdatedAt = s.now().UTC()
	s.permits[permit.ID] = permit
	s.recordVersion(permit, "isolation-confirmed")
	return permit, nil
}

func (s *PermitService) Get(permitID string) (Permit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	permit, exists := s.permits[permitID]
	return permit, exists
}

func (s *PermitService) Circuit(stationID, circuitID string) (*feed.Circuit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	circuit, exists := s.circuits[circuitKey(stationID, circuitID)]
	return circuit, exists
}

func (s *PermitService) Ground(permitID string) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return Permit{}, fmt.Errorf("permit %s not found", permitID)
	}
	if err := s.checkLive(permit); err != nil {
		return permit, err
	}
	circuit := s.circuits[circuitKey(permit.StationID, permit.CircuitID)]
	if _, err := s.isolation.Ground(circuit, permit.OperationID, s.now()); err != nil {
		return permit, err
	}
	permit.State = PermitGrounded
	permit.UpdatedAt = s.now().UTC()
	s.permits[permit.ID] = permit
	s.recordVersion(permit, "grounded")
	return permit, nil
}

func (s *PermitService) Release(permitID string) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return Permit{}, fmt.Errorf("permit %s not found", permitID)
	}
	if err := s.checkLive(permit); err != nil {
		return permit, err
	}
	circuit := s.circuits[circuitKey(permit.StationID, permit.CircuitID)]
	if _, err := s.isolation.Release(circuit, permit.OperationID, s.now()); err != nil {
		return permit, err
	}
	permit.State = PermitReleased
	permit.UpdatedAt = s.now().UTC()
	s.permits[permit.ID] = permit
	s.recordVersion(permit, "released")
	return permit, nil
}

func (s *PermitService) Renew(permitID string, generation uint64) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return Permit{}, fmt.Errorf("permit %s not found", permitID)
	}
	if permit.State == PermitReleased {
		return permit, fmt.Errorf("permit %s is released and cannot be renewed", permitID)
	}
	if err := s.checkLive(permit); err != nil {
		return permit, err
	}
	if generation != permit.Generation {
		return permit, fmt.Errorf("operation generation mismatch: permit is at generation %d", permit.Generation)
	}
	permit.Generation++
	permit.ValidFrom, permit.ExpiresAt, permit.RemindAt = s.validityWindow(s.now())
	permit.UpdatedAt = s.now().UTC()
	s.permits[permit.ID] = permit
	s.recordVersion(permit, "renewed")
	return permit, nil
}

func (s *PermitService) Versions(permitID string) ([]PermitVersion, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	history, exists := s.versions[permitID]
	if !exists {
		return nil, false
	}
	result := make([]PermitVersion, len(history))
	copy(result, history)
	return result, true
}

func (s *PermitService) View(permit Permit) PermitView {
	now := s.now().UTC()
	remaining := int64(permit.ExpiresAt.Sub(now).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return PermitView{Permit: permit, Expiry: s.expiry(permit, now), ExpiresInSeconds: remaining}
}

func (s *PermitService) Expiring() []PermitView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now().UTC()
	var result []PermitView
	for _, permit := range s.permits {
		if permit.State == PermitReleased || s.expiry(permit, now) != ExpiryExpiring {
			continue
		}
		result = append(result, s.View(permit))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ExpiresAt.Before(result[j].ExpiresAt) })
	return result
}

func (s *PermitService) validityWindow(now time.Time) (validFrom, expiresAt, remindAt time.Time) {
	validFrom = now.UTC()
	expiresAt = validFrom.Add(s.ttl)
	remindAt = expiresAt.Add(-s.remindBefore)
	return validFrom, expiresAt, remindAt
}

func (s *PermitService) expiry(permit Permit, now time.Time) ExpiryStatus {
	if !now.Before(permit.ExpiresAt) {
		return ExpiryExpired
	}
	if !now.Before(permit.RemindAt) {
		return ExpiryExpiring
	}
	return ExpiryActive
}

func (s *PermitService) checkLive(permit Permit) error {
	if s.expiry(permit, s.now().UTC()) == ExpiryExpired {
		return fmt.Errorf("permit %s expired at %s", permit.ID, permit.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

func (s *PermitService) recordVersion(permit Permit, reason string) {
	history := s.versions[permit.ID]
	s.versions[permit.ID] = append(history, PermitVersion{
		Version: uint64(len(history)) + 1, PermitID: permit.ID, State: permit.State,
		OperationID: permit.OperationID, Generation: permit.Generation,
		ValidFrom: permit.ValidFrom, ExpiresAt: permit.ExpiresAt,
		Reason: reason, RecordedAt: s.now().UTC(),
	})
}
