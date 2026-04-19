package handler

import (
	"sync"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
)

// Hub maintains the set of active clients and routes messages between them.
// It is the central nerve of the WebSocket system. Only the Run() goroutine
// writes to the clients map. External reads go through RLock-protected accessors.
type Hub struct {
	// clients maps userID -> *Client for all connected users.
	// WRITE access: only inside Run() goroutine (no lock needed for writes).
	// READ access: through RLock-protected methods like GetClient, IsOnline.
	clients map[string]*Client

	// register is a channel for	 clients requesting to join the hub.
	// The readPump or WebSocket handler sends the client here after upgrade.
	register chan *Client

	// unregister is a channel for clients requesting to leave the hub.
	// The readPump sends the client here on disconnect (clean or dirty).
	unregister chan *Client

	// mu protects read access to the clients map from outside the Run goroutine.
	// Run() itself does not need to acquire this lock for writes because it is
	// the sole writer -- but it DOES acquire a write lock so that concurrent
	// readers see a consistent snapshot.
	mu sync.RWMutex
}

// ConnRegistry is an alias for Hub, used by the server wiring layer.
type ConnRegistry = Hub

// NewConnRegistry creates a ConnRegistry (Hub) ready to run.
func NewConnRegistry() *ConnRegistry { return NewHub() }

// NewHub creates a Hub with initialized channels and map.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// Run is the Hub's main loop. Start it as a goroutine: go hub.Run()
//
// It listens on two channels:
//   - register: a new client connected. Add them to the map.
//   - unregister: a client disconnected. Remove them from the map and close
//     their send channel (which will cause writePump to exit).
//
// This is the ONLY goroutine that mutates the clients map.
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()

			// If this user already has an active connection, close the old one.
			// This handles the case where a user opens a second tab or reconnects
			// after a dirty disconnect before the server detects it.
			if existing, ok := h.clients[client.userID]; ok {
				logger.Log.Warn().
					Str("userID", client.userID).
					Msg("duplicate connection, closing old one")
				close(existing.send)
				delete(h.clients, client.userID)
			}

			h.clients[client.userID] = client
			h.mu.Unlock()

			logger.Log.Info().
				Str("userID", client.userID).
				Int("total_clients", len(h.clients)).
				Msg("client registered")

		case client := <-h.unregister:
			h.mu.Lock()
			// Only delete if the client in the map is the SAME pointer.
			// This prevents a race where a new connection for the same userID
			// registered after this client was queued for unregister.
			if existing, ok := h.clients[client.userID]; ok && existing == client {
				close(client.send)
				delete(h.clients, client.userID)
				logger.Log.Info().
					Str("userID", client.userID).
					Int("total_clients", len(h.clients)).
					Msg("client unregistered")
			}
			h.mu.Unlock()
		}
	}
}

// GetClient returns the Client for the given userID, or nil if not connected.
// Safe to call from any goroutine (uses RLock).
func (h *Hub) GetClient(userID string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.clients[userID]
}

// IsOnline returns true if the user has an active WebSocket connection.
// Safe to call from any goroutine (uses RLock).
func (h *Hub) IsOnline(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[userID]
	return ok
}