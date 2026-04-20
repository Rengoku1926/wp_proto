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
	pubsubRepo   *repository.PubSubRepo
	msgRepo      *repository.MessageRepo
	stateService *service.StateService
	offlineStore *repository.OfflineStore
}

func NewWSHandler(registry *ConnRegistry, pubsubRepo *repository.PubSubRepo, msgRepo *repository.MessageRepo, stateService *service.StateService, offlineStore *repository.OfflineStore) *WSHandler {
	return &WSHandler{hub: registry, pubsubRepo: pubsubRepo, msgRepo: msgRepo, stateService: stateService, offlineStore: offlineStore}
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

	client := NewClient(h.hub, conn, userID, h.pubsubRepo, h.msgRepo, h.stateService, h.offlineStore)
	client.hub.register <- client

	go client.writePump()
	go client.readPump()
	go client.subscribeLoop()
}
