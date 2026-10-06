package repair

import (
	"strings"
	"testing"
	"time"

	"github.com/wyw14/cry-103/internal/feed"
	"github.com/wyw14/cry-103/internal/landing"
	"github.com/wyw14/cry-103/internal/platform"
)

type mutableClock struct {
	now time.Time
}

func (c *mutableClock) Now() time.Time {
	return c.now
}

func (c *mutableClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func newPermitFixture(t *testing.T, ttl, remindBefore time.Duration) (*PermitService, *landing.ReceiptRegistry, *mutableClock) {
	t.Helper()
	clock := &mutableClock{now: time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)}
	receipts := landing.NewReceiptRegistry()
	service := NewPermitService(feed.NewIsolationService(), receipts, platform.UUIDGenerator{}, clock.Now, ttl, remindBefore)
	circuit, err := feed.NewCircuit("A", "east", clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	service.RegisterCircuit(circuit)
	return service, receipts, clock
}

func isolateAndConfirm(t *testing.T, service *PermitService, receipts *landing.ReceiptRegistry) Permit {
	t.Helper()
	permit, err := service.Request("east", "A")
	if err != nil {
		t.Fatal(err)
	}
	receipts.Record(landing.IsolationReceipt{
		StationID: permit.StationID, CircuitID: permit.CircuitID, OperationID: permit.OperationID,
		Isolated: true, ReceivedAt: permit.UpdatedAt,
	})
	permit, err = service.Evaluate(permit.ID)
	if err != nil {
		t.Fatal(err)
	}
	return permit
}

func TestRequestSetsValidityWindow(t *testing.T) {
	service, _, clock := newPermitFixture(t, 2*time.Hour, 15*time.Minute)
	permit, err := service.Request("east", "A")
	if err != nil {
		t.Fatal(err)
	}
	if permit.Generation != 1 {
		t.Fatalf("generation=%d, want 1", permit.Generation)
	}
	if !permit.ValidFrom.Equal(clock.Now().UTC()) {
		t.Fatalf("valid_from=%s, want %s", permit.ValidFrom, clock.Now().UTC())
	}
	if got := permit.ExpiresAt.Sub(permit.ValidFrom); got != 2*time.Hour {
		t.Fatalf("validity=%s, want 2h", got)
	}
	if got := permit.ExpiresAt.Sub(permit.RemindAt); got != 15*time.Minute {
		t.Fatalf("reminder lead=%s, want 15m", got)
	}
	if view := service.View(permit); view.Expiry != ExpiryActive {
		t.Fatalf("expiry=%s, want active", view.Expiry)
	}
}

func TestExpiringReminderWindow(t *testing.T) {
	service, _, clock := newPermitFixture(t, time.Hour, 10*time.Minute)
	permit, err := service.Request("east", "A")
	if err != nil {
		t.Fatal(err)
	}
	if items := service.Expiring(); len(items) != 0 {
		t.Fatalf("expiring=%d, want 0 before reminder window", len(items))
	}
	clock.Advance(51 * time.Minute)
	if view := service.View(permit); view.Expiry != ExpiryExpiring {
		t.Fatalf("expiry=%s, want expiring", view.Expiry)
	}
	items := service.Expiring()
	if len(items) != 1 || items[0].ID != permit.ID {
		t.Fatalf("expiring=%+v, want the permit", items)
	}
	if items[0].ExpiresInSeconds != int64((9 * time.Minute).Seconds()) {
		t.Fatalf("expires_in=%d, want %d", items[0].ExpiresInSeconds, int64((9 * time.Minute).Seconds()))
	}
	clock.Advance(10 * time.Minute)
	if view := service.View(permit); view.Expiry != ExpiryExpired || view.ExpiresInSeconds != 0 {
		t.Fatalf("view=%+v, want expired with zero remaining", view)
	}
	if items := service.Expiring(); len(items) != 0 {
		t.Fatalf("expiring=%d, want 0 after expiry", len(items))
	}
}

func TestExpiredPermitCannotGround(t *testing.T) {
	service, receipts, clock := newPermitFixture(t, time.Hour, 10*time.Minute)
	permit := isolateAndConfirm(t, service, receipts)
	clock.Advance(61 * time.Minute)
	if _, err := service.Ground(permit.ID); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("ground err=%v, want expiry rejection", err)
	}
	// Renewal is also closed after expiry: the permit stays unusable.
	if _, err := service.Renew(permit.ID, permit.Generation); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("renew err=%v, want expiry rejection", err)
	}
}

func TestExpiredGroundedPermitCannotRelease(t *testing.T) {
	service, receipts, clock := newPermitFixture(t, time.Hour, 10*time.Minute)
	permit := isolateAndConfirm(t, service, receipts)
	if _, err := service.Ground(permit.ID); err != nil {
		t.Fatal(err)
	}
	clock.Advance(61 * time.Minute)
	if _, err := service.Release(permit.ID); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("release err=%v, want expiry rejection", err)
	}
}

func TestReleaseHappyPath(t *testing.T) {
	service, receipts, _ := newPermitFixture(t, time.Hour, 10*time.Minute)
	permit := isolateAndConfirm(t, service, receipts)
	if _, err := service.Ground(permit.ID); err != nil {
		t.Fatal(err)
	}
	released, err := service.Release(permit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.State != PermitReleased {
		t.Fatalf("state=%s, want released", released.State)
	}
	if _, err := service.Renew(permit.ID, released.Generation); err == nil {
		t.Fatal("renew on released permit should fail")
	}
}

func TestRenewRequiresCurrentGeneration(t *testing.T) {
	service, receipts, _ := newPermitFixture(t, time.Hour, 10*time.Minute)
	permit := isolateAndConfirm(t, service, receipts)
	if _, err := service.Renew(permit.ID, permit.Generation+1); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("renew err=%v, want generation mismatch", err)
	}
}

func TestRenewPreservesIsolationEvidence(t *testing.T) {
	service, receipts, clock := newPermitFixture(t, time.Hour, 10*time.Minute)
	permit := isolateAndConfirm(t, service, receipts)
	clock.Advance(50 * time.Minute)

	renewed, err := service.Renew(permit.ID, permit.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Generation != permit.Generation+1 {
		t.Fatalf("generation=%d, want %d", renewed.Generation, permit.Generation+1)
	}
	if renewed.OperationID != permit.OperationID {
		t.Fatal("renewal must keep the isolation operation")
	}
	if renewed.State != PermitGroundable {
		t.Fatalf("state=%s, want groundable preserved", renewed.State)
	}
	if !renewed.ExpiresAt.After(permit.ExpiresAt) {
		t.Fatal("renewal must extend the validity window")
	}
	// The completed isolation evidence still matches the same operation.
	if _, matched := receipts.Match(permit.StationID, permit.CircuitID, permit.OperationID); !matched {
		t.Fatal("isolation receipt must remain valid after renewal")
	}
	// Grounding proceeds on the renewed permit without re-isolation.
	grounded, err := service.Ground(permit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grounded.State != PermitGrounded {
		t.Fatalf("state=%s, want grounded", grounded.State)
	}
	circuit, _ := service.Circuit("east", "A")
	if snapshot := circuit.Snapshot(); snapshot.State != feed.StateGrounded || snapshot.OperationID != permit.OperationID {
		t.Fatalf("circuit=%+v, want grounded under original operation", snapshot)
	}
}

func TestVersionHistoryIsQueryable(t *testing.T) {
	service, receipts, clock := newPermitFixture(t, time.Hour, 10*time.Minute)
	permit := isolateAndConfirm(t, service, receipts)
	clock.Advance(50 * time.Minute)
	if _, err := service.Renew(permit.ID, permit.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ground(permit.ID); err != nil {
		t.Fatal(err)
	}
	versions, exists := service.Versions(permit.ID)
	if !exists {
		t.Fatal("versions must be queryable")
	}
	if len(versions) != 4 {
		t.Fatalf("versions=%d, want 4 (requested, isolation-confirmed, renewed, grounded)", len(versions))
	}
	for i, version := range versions {
		if version.Version != uint64(i+1) {
			t.Fatalf("version[%d].Version=%d, want %d", i, version.Version, i+1)
		}
		if version.OperationID != permit.OperationID {
			t.Fatalf("version[%d] operation changed: %s", i, version.OperationID)
		}
	}
	if versions[2].Reason != "renewed" || versions[2].Generation != 2 {
		t.Fatalf("renewal version=%+v", versions[2])
	}
	if !versions[2].ExpiresAt.After(versions[0].ExpiresAt) {
		t.Fatal("renewal version must record the extended expiry")
	}
	if versions[0].Generation != 1 || versions[0].Reason != "requested" {
		t.Fatalf("initial version=%+v", versions[0])
	}
	if _, exists := service.Versions("permit-missing"); exists {
		t.Fatal("unknown permit must not have versions")
	}
}
