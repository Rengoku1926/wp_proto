package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/google/uuid"
)

type MessageHandler struct {
	messageSvc *service.MessageService
}

func NewMessageHandler(svc *service.MessageService) *MessageHandler {
	return &MessageHandler{messageSvc: svc}
}

func parsePaginationParams(r *http.Request) (time.Time, int) {
	cursor := time.Now().UTC()
	if s := r.URL.Query().Get("cursor"); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			cursor = t
		}
	}

	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	return cursor, limit
}

// GetConversationHistory handles GET /api/messages/dm/{user_id}?userId=<caller>&cursor=<ts>&limit=<n>
func (h *MessageHandler) GetConversationHistory(w http.ResponseWriter, r *http.Request) {
	callerID, err := uuid.Parse(r.URL.Query().Get("userId"))
	if err != nil {
		http.Error(w, `{"error":"missing or invalid userId"}`, http.StatusBadRequest)
		return
	}

	otherUserID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		http.Error(w, `{"error":"invalid user_id in path"}`, http.StatusBadRequest)
		return
	}

	cursor, limit := parsePaginationParams(r)

	resp, err := h.messageSvc.GetConversationHistory(r.Context(), callerID, otherUserID, cursor, limit)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch messages"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// GetGroupHistory handles GET /api/messages/group/{group_id}?userId=<caller>&cursor=<ts>&limit=<n>
func (h *MessageHandler) GetGroupHistory(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("userId") == "" {
		http.Error(w, `{"error":"missing userId"}`, http.StatusBadRequest)
		return
	}

	groupID, err := uuid.Parse(r.PathValue("group_id"))
	if err != nil {
		http.Error(w, `{"error":"invalid group_id in path"}`, http.StatusBadRequest)
		return
	}

	cursor, limit := parsePaginationParams(r)

	resp, err := h.messageSvc.GetGroupHistory(r.Context(), groupID, cursor, limit)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch messages"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
