package api

import (
	"net/http"
	"time"

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
	permit, err := s.services.Permits.Request(stationID, input.CircuitID, time.Duration(input.ValiditySeconds)*time.Second)
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if err := s.services.Persist("feed-isolation", permit.ID, permit); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, permit)
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
