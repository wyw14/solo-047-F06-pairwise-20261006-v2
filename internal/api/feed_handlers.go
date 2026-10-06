package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-103/internal/landing"
)

func (s *Server) startIsolation(writer http.ResponseWriter, request *http.Request) {
	stationID := chi.URLParam(request, "id")
	var input IsolationRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	permit, err := s.services.Permits.Request(stationID, input.CircuitID)
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if err := s.services.Persist("feed-isolation", permit.ID, permit); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, s.services.Permits.View(permit))
}

func (s *Server) recordReceipt(writer http.ResponseWriter, request *http.Request) {
	stationID := chi.URLParam(request, "id")
	var input ReceiptRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	receipt := landing.IsolationReceipt{
		StationID: stationID, CircuitID: input.CircuitID, OperationID: input.OperationID,
		Isolated: input.Isolated, ReceivedAt: s.services.Clock.Now().UTC(),
	}
	s.services.Receipts.Record(receipt)
	if err := s.services.Persist("feed-receipt", input.OperationID, receipt); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusCreated, receipt)
}

func (s *Server) listReceipts(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": s.services.Receipts.ForStation(chi.URLParam(request, "id"))})
}

func (s *Server) evaluatePermit(writer http.ResponseWriter, request *http.Request) {
	permit, err := s.services.Permits.Evaluate(chi.URLParam(request, "id"))
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if err := s.services.Persist("permit-groundable", permit.ID, permit); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, s.services.Permits.View(permit))
}

func (s *Server) getPermit(writer http.ResponseWriter, request *http.Request) {
	permit, exists := s.services.Permits.Get(chi.URLParam(request, "id"))
	if !exists {
		writeJSON(writer, http.StatusNotFound, ErrorResponse{Error: "permit not found"})
		return
	}
	writeJSON(writer, http.StatusOK, s.services.Permits.View(permit))
}

func (s *Server) groundPermit(writer http.ResponseWriter, request *http.Request) {
	permit, err := s.services.Permits.Ground(chi.URLParam(request, "id"))
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if err := s.services.Persist("permit-grounded", permit.ID, permit); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, s.services.Permits.View(permit))
}

func (s *Server) releasePermit(writer http.ResponseWriter, request *http.Request) {
	permit, err := s.services.Permits.Release(chi.URLParam(request, "id"))
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if err := s.services.Persist("permit-released", permit.ID, permit); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, s.services.Permits.View(permit))
}

func (s *Server) renewPermit(writer http.ResponseWriter, request *http.Request) {
	var input RenewRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	permit, err := s.services.Permits.Renew(chi.URLParam(request, "id"), input.Generation)
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if err := s.services.Persist("permit-renewed", permit.ID, permit); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, s.services.Permits.View(permit))
}

func (s *Server) listPermitVersions(writer http.ResponseWriter, request *http.Request) {
	versions, exists := s.services.Permits.Versions(chi.URLParam(request, "id"))
	if !exists {
		writeJSON(writer, http.StatusNotFound, ErrorResponse{Error: "permit not found"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": versions})
}

func (s *Server) listExpiringPermits(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": s.services.Permits.Expiring()})
}
