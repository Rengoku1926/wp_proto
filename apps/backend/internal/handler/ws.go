package handler

import (
	"net/http"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // TODO: restrict in production
	},
}

type WSHandler struct {
	hub          *ConnRegistry
	msgRepo      *repository.MessageRepo
	stateService *service.StateService
}

func NewWSHandler(registry *ConnRegistry, msgRepo *repository.MessageRepo, stateService *service.StateService) *WSHandler {
	return &WSHandler{hub: registry, msgRepo: msgRepo, stateService: stateService}
}

func (h *WSHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("userId")
	if userID == "" {
		http.Error(w, "missing userId", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Log.Error().Err(err).Str("userID", userID).Msg("WebSocket upgrade failed")
		return
	}

	client := NewClient(h.hub, conn, userID, h.msgRepo, h.stateService)
	client.hub.register <- client

	go client.writePump()
	go client.readPump()
}
