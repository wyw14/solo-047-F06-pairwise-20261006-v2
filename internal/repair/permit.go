package repair

import (
	"fmt"
	"sync"
	"time"

	"github.com/wyw14/cry-103/internal/feed"
	"github.com/wyw14/cry-103/internal/landing"
	"github.com/wyw14/cry-103/internal/platform"
)

const (
	DefaultPermitTTL     = 30 * time.Minute
	PermitReminderWindow = 5 * time.Minute
	MinPermitTTL         = time.Minute
	MaxPermitTTL         = 24 * time.Hour
)

type PermitState string

const (
	PermitRequested  PermitState = "requested"
	PermitIsolating  PermitState = "isolating"
	PermitGroundable PermitState = "groundable"
	PermitGrounded   PermitState = "grounded"
	PermitReleased   PermitState = "released"
)

type Permit struct {
	ID           string      `json:"id"`
	StationID    string      `json:"station_id"`
	CircuitID    string      `json:"circuit_id"`
	OperationID  string      `json:"operation_id"`
	State        PermitState `json:"state"`
	Generation   uint64      `json:"generation"`
	Version      uint64      `json:"version"`
	ValidFrom    time.Time   `json:"valid_from"`
	ExpiresAt    time.Time   `json:"expires_at"`
	RenewedCount uint64      `json:"renewed_count"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

// PermitView is the read projection of a permit with time-derived status.
type PermitView struct {
	Permit
	Expired          bool  `json:"expired"`
	ReminderDue      bool  `json:"reminder_due"`
	Renewable        bool  `json:"renewable"`
	RemainingSeconds int64 `json:"remaining_seconds"`
}

// PermitReminder notifies callers that a permit is approaching its expiry.
type PermitReminder struct {
	Permit  PermitView `json:"permit"`
	Reason  string     `json:"reason"`
	Message string     `json:"message"`
}

type PermitService struct {
	mu             sync.RWMutex
	permits        map[string]Permit
	versions       map[string][]Permit
	remindedFor    map[string]time.Time
	circuits       map[string]*feed.Circuit
	isolation      *feed.IsolationService
	receipts       *landing.ReceiptRegistry
	ids            platform.IDGenerator
	now            func() time.Time
	defaultTTL     time.Duration
	reminderWindow time.Duration
}

func NewPermitService(isolation *feed.IsolationService, receipts *landing.ReceiptRegistry, ids platform.IDGenerator, now func() time.Time, defaultTTL, reminderWindow time.Duration) *PermitService {
	return &PermitService{
		permits: make(map[string]Permit), versions: make(map[string][]Permit), remindedFor: make(map[string]time.Time),
		circuits: make(map[string]*feed.Circuit), isolation: isolation, receipts: receipts, ids: ids, now: now,
		defaultTTL: defaultTTL, reminderWindow: reminderWindow,
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

func validateTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("permit validity must be a positive duration")
	}
	if ttl < MinPermitTTL || ttl > MaxPermitTTL {
		return fmt.Errorf("permit validity must be between %s and %s", MinPermitTTL, MaxPermitTTL)
	}
	return nil
}

// commit stores the permit and appends an immutable versioned snapshot.
func (s *PermitService) commit(permit Permit) Permit {
	permit.UpdatedAt = s.now().UTC()
	s.permits[permit.ID] = permit
	s.versions[permit.ID] = append(s.versions[permit.ID], permit)
	return permit
}

func (s *PermitService) Request(stationID, circuitID string, ttl time.Duration) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ttl == 0 {
		ttl = s.defaultTTL
	}
	if err := validateTTL(ttl); err != nil {
		return Permit{}, err
	}
	circuit := s.circuits[circuitKey(stationID, circuitID)]
	if circuit == nil {
		return Permit{}, fmt.Errorf("feed circuit %s/%s not found", stationID, circuitID)
	}
	now := s.now().UTC()
	permit := Permit{
		ID: s.ids.New("permit"), StationID: stationID, CircuitID: circuitID,
		OperationID: s.ids.New("isolation"), State: PermitRequested,
		Generation: 1, Version: 1, ValidFrom: now, ExpiresAt: now.Add(ttl), UpdatedAt: now,
	}
	if _, err := s.isolation.Begin(circuit, permit.OperationID, now); err != nil {
		return Permit{}, err
	}
	permit.State = PermitIsolating
	return s.commit(permit), nil
}

// requireInForce rejects progression or renewal once the permit is expired or released.
func (s *PermitService) requireInForce(permit Permit, action string) error {
	if permit.State == PermitReleased {
		return fmt.Errorf("permit %s is released and cannot %s", permit.ID, action)
	}
	now := s.now()
	if !now.Before(permit.ExpiresAt) {
		return fmt.Errorf("permit %s expired at %s and cannot %s; request a new permit", permit.ID, permit.ExpiresAt, action)
	}
	return nil
}

func (s *PermitService) Evaluate(permitID string) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return Permit{}, fmt.Errorf("permit %s not found", permitID)
	}
	if err := s.requireInForce(permit, "evaluate isolation"); err != nil {
		return permit, err
	}
	circuit := s.circuits[circuitKey(permit.StationID, permit.CircuitID)]
	_, matched := s.receipts.Match(permit.StationID, permit.CircuitID, permit.OperationID)
	if _, err := s.isolation.Confirm(circuit, permit.StationID, permit.CircuitID, permit.OperationID, matched, s.now()); err != nil {
		return permit, err
	}
	permit.State = PermitGroundable
	permit.Version++
	return s.commit(permit), nil
}

func (s *PermitService) Ground(permitID string) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return Permit{}, fmt.Errorf("permit %s not found", permitID)
	}
	if permit.State != PermitGroundable {
		return permit, fmt.Errorf("permit %s must be groundable before grounding", permitID)
	}
	if err := s.requireInForce(permit, "ground"); err != nil {
		return permit, err
	}
	circuit := s.circuits[circuitKey(permit.StationID, permit.CircuitID)]
	if _, err := s.isolation.Ground(circuit, permit.OperationID, s.now()); err != nil {
		return permit, err
	}
	permit.State = PermitGrounded
	permit.Version++
	return s.commit(permit), nil
}

// Release closes the permit. Expired permits cannot be released through this flow:
// the physical release must be scheduled under a newly requested permit.
func (s *PermitService) Release(permitID string) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return Permit{}, fmt.Errorf("permit %s not found", permitID)
	}
	if permit.State != PermitGrounded {
		return permit, fmt.Errorf("permit %s must be grounded before release", permitID)
	}
	if err := s.requireInForce(permit, "release"); err != nil {
		return permit, err
	}
	circuit := s.circuits[circuitKey(permit.StationID, permit.CircuitID)]
	if _, err := s.isolation.Release(circuit, permit.OperationID, s.now()); err != nil {
		return permit, err
	}
	permit.State = PermitReleased
	permit.Version++
	return s.commit(permit), nil
}

// Renew extends the validity window while the permit is in force. The caller must
// present the current operation generation; a stale generation is rejected. Renewal
// never reissues the isolation operation: OperationID, circuit state and receipts
// are untouched so completed isolation evidence remains valid.
func (s *PermitService) Renew(permitID string, generation uint64, ttl time.Duration) (Permit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return Permit{}, fmt.Errorf("permit %s not found", permitID)
	}
	if permit.State == PermitReleased {
		return permit, fmt.Errorf("permit %s is released and cannot be renewed", permitID)
	}
	now := s.now()
	if !now.Before(permit.ExpiresAt) {
		return permit, fmt.Errorf("permit %s expired at %s and cannot be renewed; request a new permit", permitID, permit.ExpiresAt)
	}
	if generation == 0 || generation != permit.Generation {
		return permit, fmt.Errorf("permit %s renewal generation conflict: presented %d, current %d", permitID, generation, permit.Generation)
	}
	if ttl == 0 {
		ttl = s.defaultTTL
	}
	if err := validateTTL(ttl); err != nil {
		return permit, err
	}
	permit.ExpiresAt = now.UTC().Add(ttl)
	permit.Generation++
	permit.Version++
	permit.RenewedCount++
	return s.commit(permit), nil
}

func (s *PermitService) Get(permitID string) (Permit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	permit, exists := s.permits[permitID]
	return permit, exists
}

func (s *PermitService) GetView(permitID string) (PermitView, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return PermitView{}, false
	}
	return permitView(permit, s.now(), s.reminderWindow), true
}

func (s *PermitService) Circuit(stationID, circuitID string) (*feed.Circuit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	circuit, exists := s.circuits[circuitKey(stationID, circuitID)]
	return circuit, exists
}

// History returns the immutable version snapshots of a permit, oldest first.
func (s *PermitService) History(permitID string) ([]Permit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	history, exists := s.versions[permitID]
	if !exists {
		return nil, false
	}
	result := make([]Permit, len(history))
	copy(result, history)
	return result, true
}

// VersionAt returns a specific historical version snapshot.
func (s *PermitService) VersionAt(permitID string, version uint64) (Permit, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, permit := range s.versions[permitID] {
		if permit.Version == version {
			return permit, true
		}
	}
	return Permit{}, false
}

// DueReminders returns due reminders for one permit and marks its current expiry
// as notified. After renewal the new expiry becomes eligible again.
func (s *PermitService) DueReminders(permitID string) []PermitReminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	permit, exists := s.permits[permitID]
	if !exists {
		return nil
	}
	now := s.now()
	if permit.State == PermitReleased || !now.Before(permit.ExpiresAt) {
		return nil
	}
	if permit.ExpiresAt.Sub(now) > s.reminderWindow {
		return nil
	}
	if notified, ok := s.remindedFor[permitID]; ok && notified.Equal(permit.ExpiresAt) {
		return nil
	}
	s.remindedFor[permitID] = permit.ExpiresAt
	return []PermitReminder{{
		Permit: permitView(permit, now, s.reminderWindow), Reason: "permit_expiry_due",
		Message: fmt.Sprintf("permit %s expires at %s", permitID, permit.ExpiresAt.Format(time.RFC3339)),
	}}
}

func permitView(permit Permit, now time.Time, window time.Duration) PermitView {
	expired := !now.Before(permit.ExpiresAt)
	remaining := permit.ExpiresAt.Sub(now)
	seconds := int64(remaining.Seconds())
	if seconds < 0 {
		seconds = 0
	}
	active := permit.State != PermitReleased
	return PermitView{
		Permit:           permit,
		Expired:          expired,
		ReminderDue:      active && !expired && remaining <= window,
		Renewable:        active && !expired,
		RemainingSeconds: seconds,
	}
}
