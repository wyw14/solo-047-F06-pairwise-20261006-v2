package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-103/internal/repair"
)

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
	writeJSON(writer, http.StatusOK, permit)
}

func (s *Server) getPermit(writer http.ResponseWriter, request *http.Request) {
	view, exists := s.services.Permits.GetView(chi.URLParam(request, "id"))
	if !exists {
		writeJSON(writer, http.StatusNotFound, ErrorResponse{Error: "permit not found"})
		return
	}
	writeJSON(writer, http.StatusOK, view)
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
	writeJSON(writer, http.StatusOK, permit)
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
	writeJSON(writer, http.StatusOK, permit)
}

func (s *Server) renewPermit(writer http.ResponseWriter, request *http.Request) {
	var input RenewPermitRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	permit, err := s.services.Permits.Renew(
		chi.URLParam(request, "id"), input.Generation, time.Duration(input.ValiditySeconds)*time.Second,
	)
	if err != nil {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if err := s.services.Persist("permit-renewed", permit.ID, permit); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, permit)
}

func (s *Server) listPermitHistory(writer http.ResponseWriter, request *http.Request) {
	history, exists := s.services.Permits.History(chi.URLParam(request, "id"))
	if !exists {
		writeJSON(writer, http.StatusNotFound, ErrorResponse{Error: "permit not found"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": history})
}

func (s *Server) getPermitVersion(writer http.ResponseWriter, request *http.Request) {
	version, err := strconv.ParseUint(chi.URLParam(request, "version"), 10, 64)
	if err != nil || version == 0 {
		writeJSON(writer, http.StatusBadRequest, ErrorResponse{Error: "version must be a positive integer"})
		return
	}
	permit, exists := s.services.Permits.VersionAt(chi.URLParam(request, "id"), version)
	if !exists {
		writeJSON(writer, http.StatusNotFound, ErrorResponse{Error: "permit version not found"})
		return
	}
	writeJSON(writer, http.StatusOK, permit)
}

func (s *Server) listPermitReminders(writer http.ResponseWriter, request *http.Request) {
	permitID := chi.URLParam(request, "id")
	items := s.services.Permits.DueReminders(permitID)
	if items == nil {
		// Distinguish an unknown permit from a permit with no due reminder.
		if _, exists := s.services.Permits.Get(permitID); !exists {
			writeJSON(writer, http.StatusNotFound, ErrorResponse{Error: "permit not found"})
			return
		}
		items = []repair.PermitReminder{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items})
}
