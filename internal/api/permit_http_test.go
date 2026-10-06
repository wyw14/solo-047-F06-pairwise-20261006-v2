package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-103/internal/feed"
	"github.com/wyw14/cry-103/internal/landing"
	"github.com/wyw14/cry-103/internal/repair"
	"github.com/wyw14/cry-103/internal/store"
)

type adjustableClock struct{ now time.Time }

func (c *adjustableClock) Now() time.Time { return c.now }

func permitHTTPServer(t *testing.T) (*Server, *adjustableClock) {
	t.Helper()
	clock := &adjustableClock{now: time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)}
	receipts := landing.NewReceiptRegistry()
	permits := repair.NewPermitService(feed.NewIsolationService(), receipts, staticIDs{}, clock.Now, repair.DefaultPermitTTL, 2*time.Minute)
	circuit, err := feed.NewCircuit("B", "east", clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	permits.RegisterCircuit(circuit)
	services := &Services{
		Clock: clock, IDs: staticIDs{},
		Journal:  store.NewJournal(filepath.Join(t.TempDir(), "events.jsonl")),
		Permits:  permits,
		Receipts: receipts,
	}
	return NewServer(services), clock
}

type staticIDs struct{}

func (staticIDs) New(prefix string) string { return prefix + "-fixed" }

func serve(server *Server, method, path string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(method, path, reader))
	return recorder
}

func TestPermitHTTPRenewExpiryAndHistoryFlow(t *testing.T) {
	server, clock := permitHTTPServer(t)

	// Request with a 10 minute validity window.
	created := serve(server, http.MethodPost, "/api/landings/east/feed-isolations", IsolationRequest{
		CircuitID: "B", ValiditySeconds: 600,
	})
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var permit repair.Permit
	if err := json.Unmarshal(created.Body.Bytes(), &permit); err != nil {
		t.Fatal(err)
	}
	if permit.Generation != 1 || permit.Version != 1 {
		t.Fatalf("unexpected permit: %+v", permit)
	}

	// Stale operation generation is rejected.
	conflict := serve(server, http.MethodPost, fmt.Sprintf("/api/permits/%s/renew", permit.ID), RenewPermitRequest{
		Generation: 42, ValiditySeconds: 600,
	})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("renew conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	// Current generation renews and bumps the generation/version.
	renewed := serve(server, http.MethodPost, fmt.Sprintf("/api/permits/%s/renew", permit.ID), RenewPermitRequest{
		Generation: 1, ValiditySeconds: 600,
	})
	if renewed.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", renewed.Code, renewed.Body.String())
	}
	var next repair.Permit
	if err := json.Unmarshal(renewed.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if next.Generation != 2 || next.Version != 2 || next.RenewedCount != 1 {
		t.Fatalf("unexpected renewed permit: %+v", next)
	}
	if next.OperationID != permit.OperationID {
		t.Fatal("renewal must preserve the isolation operation evidence")
	}

	// Reminder becomes due inside the configured window.
	clock.now = next.ExpiresAt.Add(-time.Minute)
	reminders := serve(server, http.MethodGet, fmt.Sprintf("/api/permits/%s/reminders", permit.ID), nil)
	if reminders.Code != http.StatusOK || reminders.Body.String() == "" {
		t.Fatalf("reminders status=%d body=%s", reminders.Code, reminders.Body.String())
	}
	if bytes.Count(reminders.Body.Bytes(), []byte("permit_expiry_due")) != 1 {
		t.Fatalf("expected one due reminder, body=%s", reminders.Body.String())
	}

	// After lapse, grounding is refused.
	clock.now = next.ExpiresAt.Add(time.Second)
	blocked := serve(server, http.MethodPost, fmt.Sprintf("/api/permits/%s/ground", permit.ID), nil)
	if blocked.Code != http.StatusConflict {
		t.Fatalf("expired ground status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	blockedRenew := serve(server, http.MethodPost, fmt.Sprintf("/api/permits/%s/renew", permit.ID), RenewPermitRequest{
		Generation: 2, ValiditySeconds: 600,
	})
	if blockedRenew.Code != http.StatusConflict {
		t.Fatalf("expired renew status=%d body=%s", blockedRenew.Code, blockedRenew.Body.String())
	}

	// History retains both versions; the first version shows the original generation.
	history := serve(server, http.MethodGet, fmt.Sprintf("/api/permits/%s/history", permit.ID), nil)
	if history.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", history.Code, history.Body.String())
	}
	var versions struct {
		Items []repair.Permit `json:"items"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &versions); err != nil {
		t.Fatal(err)
	}
	if len(versions.Items) != 2 {
		t.Fatalf("history len=%d want 2", len(versions.Items))
	}

	versionOne := serve(server, http.MethodGet, fmt.Sprintf("/api/permits/%s/history/1", permit.ID), nil)
	if versionOne.Code != http.StatusOK {
		t.Fatalf("version status=%d body=%s", versionOne.Code, versionOne.Body.String())
	}
	var first repair.Permit
	if err := json.Unmarshal(versionOne.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Generation != 1 || first.RenewedCount != 0 {
		t.Fatalf("historical snapshot mutated: %+v", first)
	}

	missingVersion := serve(server, http.MethodGet, fmt.Sprintf("/api/permits/%s/history/99", permit.ID), nil)
	if missingVersion.Code != http.StatusNotFound {
		t.Fatalf("missing version status=%d", missingVersion.Code)
	}
}
