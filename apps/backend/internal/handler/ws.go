package handler

import (
	"log"
	"net/http"
	"sync"
	"github.com/gorilla/websocket"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/google/uuid"
)

//thread safe map of online user connection
type ConnRegistry struct{
	mu sync.RWMutex
	clients map[uuid.UUID]*Client
}

func NewConnRegistry() *ConnRegistry {
	return &ConnRegistry{
		clients: make(map[uuid.UUID]*Client),
	}
}

func (r *ConnRegistry) Set(userID uuid.UUID, c *Client){
	r.mu.Lock()
	//if existing connection exists then close the connection 
	if old, ok := r.clients[userID]; ok {
		close(old.send)
	}
	r.clients[userID] = c
}

func (r *ConnRegistry) Get(userID uuid.UUID) *Client{
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clients[userID]
}

func (r *ConnRegistry) Remove(userID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[userID]; ok {
		close(c.send)
		delete(r.clients, userID)
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Allow all origins for local dev. Lock this down in production.
	CheckOrigin: func(r *http.Request) bool { return true },
}

type WSHandler struct{
	Registry *ConnRegistry
	MsgRepo *repository.MessageRepo
}

func NewWSHandler(registry *ConnRegistry, msgRepo *repository.MessageRepo) *WSHandler {
	return &WSHandler{
		Registry: registry,
		MsgRepo: msgRepo,
	}
}

func (h *WSHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request){
	userIDStr := r.URL.Query().Get("user_id")
	if userIDStr == "" {
		http.Error(w, "missing user_id query param", http.StatusBadRequest)
		return
	}

	userID, err := uuid.Parse(userIDStr)
	if err != nil {
		http.Error(w, "invalid user_id: must be a UUID", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade failed: %v", err)
		return
	}

	client := &Client{
		conn:     conn,
		userID:   userID,
		send:     make(chan []byte, 256),
		msgRepo:  h.MsgRepo,
		registry: h.Registry,
	}

	h.Registry.Set(userID, client)

	// Start the read and write pumps in their own goroutines.
	go client.writePump()
	go client.readPump()

	log.Printf("user %s connected via WebSocket", userID)
}