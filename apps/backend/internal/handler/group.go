package handler

import (
	"encoding/json"
	"net/http"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// GroupHandler handles HTTP endpoints for group management.
type GroupHandler struct {
	svc *service.GroupService
}

// NewGroupHandler creates a new GroupHandler.
func NewGroupHandler(svc *service.GroupService) *GroupHandler {
	return &GroupHandler{svc: svc}
}

// CreateGroup handles POST /api/groups.
//
// Request body: {"name": "My Group"}
// Response: the created group object.
func (h *GroupHandler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	group, err := h.svc.CreateGroup(r.Context(), req.Name)
	if err != nil {
		log.Error().Err(err).Msg("failed to create group")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(group)
}

// AddMember handles POST /api/groups/{id}/members.
//
// Request body: {"user_id": "uuid-here"}
func (h *GroupHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	groupIDStr := r.PathValue("id")
	groupID, err := uuid.Parse(groupIDStr)
	if err != nil {
		http.Error(w, "invalid group id", http.StatusBadRequest)
		return
	}

	var req struct {
		UserID uuid.UUID `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.svc.AddMember(r.Context(), groupID, req.UserID); err != nil {
		log.Error().Err(err).Msg("failed to add member")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ListMembers handles GET /api/groups/{id}/members.
func (h *GroupHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	groupIDStr := r.PathValue("id")
	groupID, err := uuid.Parse(groupIDStr)
	if err != nil {
		http.Error(w, "invalid group id", http.StatusBadRequest)
		return
	}

	members, err := h.svc.ListMembers(r.Context(), groupID)
	if err != nil {
		log.Error().Err(err).Msg("failed to list members")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(members)
}