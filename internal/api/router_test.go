package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-103/internal/feed"
	"github.com/wyw14/cry-103/internal/incident"
	"github.com/wyw14/cry-103/internal/landing"
	"github.com/wyw14/cry-103/internal/otdr"
	"github.com/wyw14/cry-103/internal/platform"
	"github.com/wyw14/cry-103/internal/repair"
	"github.com/wyw14/cry-103/internal/repeater"
	"github.com/wyw14/cry-103/internal/route"
	"github.com/wyw14/cry-103/internal/span"
	"github.com/wyw14/cry-103/internal/store"
	"github.com/wyw14/cry-103/internal/switchover"
	"github.com/wyw14/cry-103/internal/telemetry"
	"github.com/wyw14/cry-103/internal/topology"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	clock := fixedClock{now: time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)}
	ids := platform.UUIDGenerator{}
	registry := topology.NewRegistry()
	if _, err := registry.Register("S3", topology.PathPrimary, clock.Now()); err != nil {
		t.Fatal(err)
	}
	target, err := span.New("S3", "S3", 126, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	path, err := route.New("R-S3", "S3", clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	drainer := route.NewDrainer(0)
	activator := route.NewActivator(registry)
	receipts := landing.NewReceiptRegistry()
	permitService := repair.NewPermitService(feed.NewIsolationService(), receipts, ids, clock.Now, time.Hour, 10*time.Minute)
	circuit, err := feed.NewCircuit("B", "east", clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	permitService.RegisterCircuit(circuit)
	repeaterAsset, err := repeater.New("R07", clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := incident.NewRepeaterLifecycle()
	projector := repeater.NewHealthProjector(lifecycle, 85)
	projector.Register(repeaterAsset)
	repository := telemetry.NewRepository()
	acceptance := repair.NewAcceptanceService(switchover.NewRestorer(drainer, activator, registry, ids, clock.Now), clock.Now)
	acceptance.Register("S3", target, path)
	services := &Services{
		Clock: clock, IDs: ids, Journal: store.NewJournal(filepath.Join(t.TempDir(), "events.jsonl")),
		Spans: map[string]*span.Span{"S3": target}, Routes: map[string]*route.Route{"R-S3": path},
		Repeaters: map[string]*repeater.Repeater{"R07": repeaterAsset}, Topology: registry,
		OTDR:        otdr.NewSessionService(registry, landing.NewScanner(registry, 0), ids, clock.Now),
		Switchovers: switchover.NewSessionService(drainer, activator, ids, clock.Now, time.Second),
		Permits:     permitService, Receipts: receipts, Acceptance: acceptance,
		Telemetry: telemetry.NewIngestor(repository, clock.Now, projector), TelemetryRepository: repository,
		Admission: incident.NewAdmissionService(telemetry.NewMuteManager(clock.Now)),
	}
	return NewServer(services)
}

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

func TestPermitRenewalHTTPFlow(t *testing.T) {
	server := testServer(t)

	isolation, _ := json.Marshal(IsolationRequest{CircuitID: "B"})
	created := httptest.NewRecorder()
	server.Handler().ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/landings/east/feed-isolations", bytes.NewReader(isolation)))
	if created.Code != http.StatusAccepted {
		t.Fatalf("isolation status=%d body=%s", created.Code, created.Body.String())
	}
	var permit repair.PermitView
	if err := json.Unmarshal(created.Body.Bytes(), &permit); err != nil {
		t.Fatal(err)
	}
	if permit.Generation != 1 || permit.Expiry != repair.ExpiryActive || permit.ExpiresAt.IsZero() {
		t.Fatalf("unexpected permit view: %+v", permit)
	}

	stale, _ := json.Marshal(RenewRequest{Generation: 99})
	conflict := httptest.NewRecorder()
	server.Handler().ServeHTTP(conflict, httptest.NewRequest(http.MethodPost, "/api/permits/"+permit.ID+"/renew", bytes.NewReader(stale)))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("stale renew status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	renew, _ := json.Marshal(RenewRequest{Generation: permit.Generation})
	renewed := httptest.NewRecorder()
	server.Handler().ServeHTTP(renewed, httptest.NewRequest(http.MethodPost, "/api/permits/"+permit.ID+"/renew", bytes.NewReader(renew)))
	if renewed.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", renewed.Code, renewed.Body.String())
	}
	var current repair.PermitView
	if err := json.Unmarshal(renewed.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Generation != 2 || current.OperationID != permit.OperationID || current.State != repair.PermitIsolating {
		t.Fatalf("unexpected renewed permit: %+v", current)
	}

	versions := httptest.NewRecorder()
	server.Handler().ServeHTTP(versions, httptest.NewRequest(http.MethodGet, "/api/permits/"+permit.ID+"/versions", nil))
	if versions.Code != http.StatusOK {
		t.Fatalf("versions status=%d body=%s", versions.Code, versions.Body.String())
	}
	var history struct {
		Items []repair.PermitVersion `json:"items"`
	}
	if err := json.Unmarshal(versions.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 2 || history.Items[1].Reason != "renewed" || history.Items[1].Generation != 2 {
		t.Fatalf("unexpected versions: %+v", history.Items)
	}

	expiring := httptest.NewRecorder()
	server.Handler().ServeHTTP(expiring, httptest.NewRequest(http.MethodGet, "/api/permits/expiring", nil))
	if expiring.Code != http.StatusOK {
		t.Fatalf("expiring status=%d body=%s", expiring.Code, expiring.Body.String())
	}
}

func TestHealthAndScanHTTPFlow(t *testing.T) {
	server := testServer(t)
	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", health.Code, health.Body.String())
	}

	body, _ := json.Marshal(ScanRequest{StationID: "east", LengthKM: 126, Chunks: 3})
	scan := httptest.NewRecorder()
	server.Handler().ServeHTTP(scan, httptest.NewRequest(http.MethodPost, "/api/spans/S3/otdr-scans", bytes.NewReader(body)))
	if scan.Code != http.StatusCreated {
		t.Fatalf("scan status=%d body=%s", scan.Code, scan.Body.String())
	}
	var trace otdr.Trace
	if err := json.Unmarshal(scan.Body.Bytes(), &trace); err != nil {
		t.Fatal(err)
	}
	if trace.SpanID != "S3" || len(trace.Chunks) != 3 {
		t.Fatalf("unexpected trace: %+v", trace)
	}

	status := httptest.NewRecorder()
	server.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/system/status", nil))
	if status.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", status.Code, status.Body.String())
	}
}
