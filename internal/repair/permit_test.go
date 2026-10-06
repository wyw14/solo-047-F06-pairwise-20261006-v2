package repair

import (
	"testing"
	"time"

	"github.com/wyw14/cry-103/internal/feed"
	"github.com/wyw14/cry-103/internal/landing"
	"github.com/wyw14/cry-103/internal/platform"
)

type mutableClock struct {
	now time.Time
}

func (c *mutableClock) Now() time.Time { return c.now }

func newTestPermitService(t *testing.T, ttl, window time.Duration) (*PermitService, *mutableClock, *landing.ReceiptRegistry) {
	t.Helper()
	clock := &mutableClock{now: time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)}
	receipts := landing.NewReceiptRegistry()
	service := NewPermitService(feed.NewIsolationService(), receipts, platform.UUIDGenerator{}, clock.Now, ttl, window)
	circuit, err := feed.NewCircuit("B", "east", clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	service.RegisterCircuit(circuit)
	return service, clock, receipts
}

func recordMatchingReceipt(t *testing.T, receipts *landing.ReceiptRegistry, permit Permit) {
	t.Helper()
	receipts.Record(landing.IsolationReceipt{
		StationID: permit.StationID, CircuitID: permit.CircuitID, OperationID: permit.OperationID,
		Isolated: true, ReceivedAt: time.Now().UTC(),
	})
}

func TestPermitRequestCarriesValidityWindow(t *testing.T) {
	service, clock, _ := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	permit, err := service.Request("east", "B", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if permit.State != PermitIsolating || permit.Generation != 1 || permit.Version != 1 {
		t.Fatalf("unexpected permit: %+v", permit)
	}
	if !permit.ValidFrom.Equal(clock.Now()) {
		t.Fatalf("valid_from=%s want %s", permit.ValidFrom, clock.Now())
	}
	if want := clock.Now().Add(10 * time.Minute); !permit.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at=%s want %s", permit.ExpiresAt, want)
	}

	view, exists := service.GetView(permit.ID)
	if !exists || view.Expired || !view.Renewable || view.RemainingSeconds != 600 {
		t.Fatalf("unexpected view: %+v exists=%v", view, exists)
	}
}

func TestPermitTTLBounds(t *testing.T) {
	service, _, _ := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	if _, err := service.Request("east", "B", 30*time.Second); err == nil {
		t.Fatal("expected sub-minute TTL to be rejected")
	}
	if _, err := service.Request("east", "B", 25*time.Hour); err == nil {
		t.Fatal("expected over-day TTL to be rejected")
	}
}

func TestExpiredPermitCannotGroundOrRenew(t *testing.T) {
	service, clock, receipts := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	permit, err := service.Request("east", "B", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recordMatchingReceipt(t, receipts, permit)
	if _, err := service.Evaluate(permit.ID); err != nil {
		t.Fatal(err)
	}

	clock.now = clock.now.Add(3 * time.Minute)
	if _, err := service.Ground(permit.ID); err == nil {
		t.Fatal("expired permit must not ground")
	}
	if _, err := service.Renew(permit.ID, 1, 10*time.Minute); err == nil {
		t.Fatal("expired permit must not renew; a new permit is required")
	}

	view, _ := service.GetView(permit.ID)
	if !view.Expired || view.Renewable {
		t.Fatalf("expected expired non-renewable view, got %+v", view)
	}
}

func TestExpiredGroundedPermitCannotBeReleased(t *testing.T) {
	service, clock, receipts := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	permit, err := service.Request("east", "B", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recordMatchingReceipt(t, receipts, permit)
	if _, err := service.Evaluate(permit.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ground(permit.ID); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(3 * time.Minute)
	if _, err := service.Release(permit.ID); err == nil {
		t.Fatal("expired grounded permit must not be released")
	}
}

func TestGroundedPermitExpiryBlocksReleaseButRenewalAllowsIt(t *testing.T) {
	service, clock, receipts := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	permit, err := service.Request("east", "B", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recordMatchingReceipt(t, receipts, permit)
	if _, err := service.Evaluate(permit.ID); err != nil {
		t.Fatal(err)
	}
	grounded, err := service.Ground(permit.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Renew just before expiry with the current operation generation.
	clock.now = clock.now.Add(119 * time.Second)
	renewed, err := service.Renew(grounded.ID, grounded.Generation, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Generation != 2 || renewed.RenewedCount != 1 {
		t.Fatalf("unexpected renewed permit: %+v", renewed)
	}
	// The old expiry would have lapsed; renewal keeps the release window open.
	clock.now = clock.now.Add(time.Minute)
	released, err := service.Release(renewed.ID)
	if err != nil {
		t.Fatalf("release after renewal failed: %v", err)
	}
	if released.State != PermitReleased {
		t.Fatalf("unexpected state: %s", released.State)
	}
}

func TestRenewalRequiresCurrentGenerationAndPreservesEvidence(t *testing.T) {
	service, _, receipts := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	permit, err := service.Request("east", "B", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recordMatchingReceipt(t, receipts, permit)
	evaluated, err := service.Evaluate(permit.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Stale and missing generations are rejected.
	if _, err := service.Renew(permit.ID, 99, 10*time.Minute); err == nil {
		t.Fatal("stale generation must be rejected")
	}
	if _, err := service.Renew(permit.ID, 0, 10*time.Minute); err == nil {
		t.Fatal("missing generation must be rejected")
	}

	renewed, err := service.Renew(permit.ID, evaluated.Generation, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Isolation evidence is immutable: operation id, receipt and circuit state survive renewal.
	if renewed.OperationID != permit.OperationID {
		t.Fatalf("operation id changed by renewal: %s -> %s", permit.OperationID, renewed.OperationID)
	}
	if _, matched := receipts.Match(renewed.StationID, renewed.CircuitID, renewed.OperationID); !matched {
		t.Fatal("isolation receipt no longer matches after renewal")
	}
	if circuit, ok := service.Circuit(renewed.StationID, renewed.CircuitID); !ok {
		t.Fatal("circuit missing")
	} else if snapshot := circuit.Snapshot(); snapshot.State != feed.StateIsolated || snapshot.OperationID != renewed.OperationID {
		t.Fatalf("isolation evidence changed: %+v", snapshot)
	}

	// Operations still progress under the same operation after renewal.
	if _, err := service.Ground(renewed.ID); err != nil {
		t.Fatalf("ground after renewal failed: %v", err)
	}
}

func TestReleasedPermitIsTerminal(t *testing.T) {
	service, _, receipts := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	permit, err := service.Request("east", "B", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recordMatchingReceipt(t, receipts, permit)
	if _, err := service.Evaluate(permit.ID); err != nil {
		t.Fatal(err)
	}
	grounded, err := service.Ground(permit.ID)
	if err != nil {
		t.Fatal(err)
	}
	released, err := service.Release(grounded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(released.ID, released.Generation, 10*time.Minute); err == nil {
		t.Fatal("released permit must not be renewable")
	}
	if _, err := service.Ground(released.ID); err == nil {
		t.Fatal("released permit must not ground again")
	}
}

func TestPermitHistoryIsQueryableAndImmutable(t *testing.T) {
	service, _, receipts := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	permit, err := service.Request("east", "B", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recordMatchingReceipt(t, receipts, permit)
	if _, err := service.Evaluate(permit.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(permit.ID, 1, 20*time.Minute); err != nil {
		t.Fatal(err)
	}

	history, exists := service.History(permit.ID)
	if !exists {
		t.Fatal("history missing")
	}
	if len(history) != 3 {
		t.Fatalf("history length=%d want 3", len(history))
	}
	for i, snapshot := range history {
		if snapshot.Version != uint64(i+1) {
			t.Fatalf("history[%d].version=%d want %d", i, snapshot.Version, i+1)
		}
	}
	first, ok := service.VersionAt(permit.ID, 1)
	if !ok || first.State != PermitIsolating || first.Generation != 1 {
		t.Fatalf("unexpected historical version: %+v ok=%v", first, ok)
	}
	if _, ok := service.VersionAt(permit.ID, 99); ok {
		t.Fatal("unknown version must not be found")
	}
	if _, ok := service.History("permit-missing"); ok {
		t.Fatal("unknown permit history must not exist")
	}
}

func TestDueRemindersFireOncePerExpiryAndRearmAfterRenewal(t *testing.T) {
	service, clock, _ := newTestPermitService(t, 30*time.Minute, 5*time.Minute)
	permit, err := service.Request("east", "B", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Outside the reminder window: nothing.
	clock.now = clock.now.Add(4 * time.Minute)
	if reminders := service.DueReminders(permit.ID); len(reminders) != 0 {
		t.Fatalf("expected no reminders, got %+v", reminders)
	}

	// Inside the window: one reminder.
	clock.now = clock.now.Add(2 * time.Minute)
	reminders := service.DueReminders(permit.ID)
	if len(reminders) != 1 || reminders[0].Reason != "permit_expiry_due" {
		t.Fatalf("unexpected reminders: %+v", reminders)
	}
	if !reminders[0].Permit.ReminderDue {
		t.Fatal("reminder permit should report reminder_due")
	}
	// Polling again does not repeat the reminder for the same expiry.
	if reminders := service.DueReminders(permit.ID); len(reminders) != 0 {
		t.Fatalf("reminder must not repeat, got %+v", reminders)
	}

	// Renewal extends the expiry and rearms the reminder.
	renewed, err := service.Renew(permit.ID, 1, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = renewed.ExpiresAt.Add(-2 * time.Minute)
	if reminders := service.DueReminders(permit.ID); len(reminders) != 1 {
		t.Fatalf("expected reminder after renewal, got %+v", reminders)
	}
}
