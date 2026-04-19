package handler

import (
	"net/http"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
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
	hub     *ConnRegistry
	msgRepo *repository.MessageRepo
}

func NewWSHandler(registry *ConnRegistry, msgRepo *repository.MessageRepo) *WSHandler {
	return &WSHandler{hub: registry, msgRepo: msgRepo}
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

	client := NewClient(h.hub, conn, userID, h.msgRepo)
	client.hub.register <- client

	go client.writePump()
	go client.readPump()
}

// HandleWebSocket is kept for backward-compat with the old hub-only wiring.
func HandleWebSocket(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		client := NewClient(hub, conn, userID, nil)
		client.hub.register <- client

		go client.writePump()
		go client.readPump()
	}
}