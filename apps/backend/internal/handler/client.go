package handler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

const (
	writeWait       = 10 * time.Second
	pongWait        = 60 * time.Second
	pingPeriod      = (pongWait * 9) / 10
	maxMessageSize  = 512 * 1024
	sendChannelSize = 256
)

// Envelope is the top-level JSON frame on the wire.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// MessagePayload is the payload for frame type "message" (client -> server).
type MessagePayload struct {
	ClientID    string `json:"client_id"`
	RecipientID string `json:"recipient_id"`
	Content     string `json:"content"`
}

// AckPayload is the payload for frame type "ack" (recipient -> server).
type AckPayload struct {
	MessageID string `json:"message_id"`
}

// ReadPayload is the payload for frame type "read" (recipient -> server).
type ReadPayload struct {
	MessageIDs []string `json:"message_ids"`
}

// StateUpdatePayload is the payload for frame type "state_update" (server -> sender).
type StateUpdatePayload struct {
	MessageID string `json:"message_id"`
	ClientID  string `json:"client_id,omitempty"`
	State     int    `json:"state"`
}

type Client struct {
	hub          *Hub
	conn         *websocket.Conn
	userID       string
	send         chan []byte
	sub          *redis.PubSub
	pubsubRepo   *repository.PubSubRepo
	msgRepo      *repository.MessageRepo
	stateService *service.StateService
	offlineStore *repository.OfflineStore
}

func (c *Client) drainOfflineBuffer() {
	ctx := context.Background()

	messages, err := c.offlineStore.Drain(ctx, c.userID)
	if err != nil {
		log.Error().Err(err).Str("user", c.userID).Msg("failed to drain offline buffer")
		return
	}

	if len(messages) == 0 {
		return
	}

	for _, msg := range messages {
		payload, err := json.Marshal(Envelope{
			Type: "message",
			Payload: mustMarshal(map[string]string{
				"message_id": msg.ID.String(),
				"sender_id":  msg.SenderID.String(),
				"content":    msg.Body,
			}),
		})
		if err != nil {
			log.Error().Err(err).Str("msg_id", msg.ID.String()).Msg("failed to marshal offline message")
			continue
		}

		// Push to the client's send channel.
		// Use select-with-default to avoid blocking if the channel is full
		// (which would mean the client is slow or disconnected).
		select {
		case c.send <- payload:
		default:
			log.Warn().
				Str("user", c.userID).
				Str("msg_id", msg.ID.String()).
				Msg("send channel full during offline drain, message dropped")
		}
	}
	

	log.Info().
		Str("user", c.userID).
		Int("count", len(messages)).
		Msg("drained offline buffer")
}

func NewClient(hub *Hub, conn *websocket.Conn, userID string, pubsubRepo *repository.PubSubRepo, msgRepo *repository.MessageRepo, stateService *service.StateService, offlineStore *repository.OfflineStore) *Client {
	return &Client{
		hub:          hub,
		conn:         conn,
		userID:       userID,
		send:         make(chan []byte, sendChannelSize),
		sub:          pubsubRepo.Subscribe(context.Background(), userID),
		pubsubRepo:   pubsubRepo,
		msgRepo:      msgRepo,
		stateService: stateService,
		offlineStore: offlineStore,
	}
}

func (c *Client) subscribeLoop() {
	ch := c.sub.Channel()
	for msg := range ch {
		c.send <- []byte(msg.Payload)
	}
	logger.Log.Debug().Str("user", c.userID).Msg("subscribeLoop exited")
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
		if c.sub != nil {
			c.sub.Close()
		}
		logger.Log.Debug().Str("userID", c.userID).Msg("readPump exited")
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				logger.Log.Warn().Err(err).Str("userID", c.userID).Msg("unexpected websocket close")
			}
			return
		}
		c.handleIncoming(context.Background(), message)
	}
}

func (c *Client) handleIncoming(ctx context.Context, raw []byte) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		logger.Log.Error().Err(err).Msg("malformed envelope")
		return
	}

	switch env.Type {
	case "message":
		c.handleMessage(ctx, env.Payload)
	case "ack":
		c.handleAck(ctx, env.Payload)
	case "read":
		c.handleRead(ctx, env.Payload)
	case "":
		// ignore empty/ping frames from some WS clients
	default:
		logger.Log.Warn().Str("type", env.Type).Str("userID", c.userID).Msg("unknown frame type")
	}
}

// handleMessage persists the message, confirms SENT to the sender, and routes to the recipient.
func (c *Client) handleMessage(ctx context.Context, raw json.RawMessage) {
	var p MessagePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		logger.Log.Error().Err(err).Msg("bad message payload")
		return
	}

	senderUUID, err := uuid.Parse(c.userID)
	if err != nil {
		logger.Log.Error().Err(err).Str("userID", c.userID).Msg("invalid sender UUID")
		return
	}
	recipientUUID, err := uuid.Parse(p.RecipientID)
	if err != nil {
		logger.Log.Error().Err(err).Str("recipient_id", p.RecipientID).Msg("invalid recipient UUID")
		return
	}
	clientUUID, err := uuid.Parse(p.ClientID)
	if err != nil {
		clientUUID = uuid.New()
	}

	msg, err := c.msgRepo.Create(ctx, &model.Message{
		ClientID:    clientUUID,
		SenderID:    senderUUID,
		RecipientID: recipientUUID,
		Body:        p.Content,
		State:       model.StatePending,
	})
	if err != nil {
		logger.Log.Error().Err(err).Msg("failed to save message")
		return
	}

	_, _ = c.stateService.Transition(ctx, msg.ID.String(), service.StateSent)

	// sentAck, _ := json.Marshal(Envelope{
	// 	Type: "state_update",
	// 	Payload: mustMarshal(StateUpdatePayload{
	// 		MessageID: msg.ID.String(),
	// 		ClientID:  clientUUID.String(),
	// 		State:     service.StateSent,
	// 	}),
	// })
	c.sendJSON("state_update", StateUpdatePayload{
		MessageID: msg.ID.String(),
		ClientID:  clientUUID.String(),
		State:     service.StateSent,
	})
	// if err := c.pubsubRepo.Publish(ctx, c.userID, sentAck); err != nil {
	// 	logger.Log.Error().Err(err).Msg("failed to publish SENT ack")
	// }

	outbound, _ := json.Marshal(Envelope{
		Type: "message",
		Payload: mustMarshal(map[string]string{
			"message_id": msg.ID.String(),
			"sender_id":  c.userID,
			"content":    p.Content,
		}),
	})

	recipientID := msg.RecipientID.String()
	if c.hub.IsOnline(recipientID) {
		if err := c.pubsubRepo.Publish(ctx, recipientID, outbound); err != nil {
			logger.Log.Warn().Err(err).Str("recipient", recipientID).Msg("pub/sub failed, falling back to offline buffer")
			if err := c.offlineStore.Push(ctx, recipientID, msg); err != nil {
				logger.Log.Error().Err(err).Str("recipient", recipientID).Msg("failed to buffer offline message")
			}
		}
	} else {
		if err := c.offlineStore.Push(ctx, recipientID, msg); err != nil {
			logger.Log.Error().Err(err).Str("recipient", recipientID).Msg("failed to buffer offline message")
		}
	}
}

// handleAck transitions the message to DELIVERED and notifies the original sender.
func (c *Client) handleAck(ctx context.Context, raw json.RawMessage) {
	var p AckPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		logger.Log.Error().Err(err).Msg("bad ack payload")
		return
	}

	newState, err := c.stateService.Transition(ctx, p.MessageID, service.StateDelivered)
	if err != nil && !errors.Is(err, service.ErrStaleTransition) {
		logger.Log.Error().Err(err).Msg("ack transition failed")
		return
	}

	msgUUID, err := uuid.Parse(p.MessageID)
	if err != nil {
		logger.Log.Error().Err(err).Str("message_id", p.MessageID).Msg("invalid message UUID in ack")
		return
	}

	msg, err := c.msgRepo.GetByID(ctx, msgUUID)
	if err != nil {
		logger.Log.Error().Err(err).Msg("failed to fetch message for ack")
		return
	}

	outbound, _ := json.Marshal(Envelope{
		Type: "state_update",
		Payload: mustMarshal(StateUpdatePayload{
			MessageID: p.MessageID,
			State:     newState,
		}),
	})
	if err := c.pubsubRepo.Publish(ctx, msg.SenderID.String(), outbound); err != nil {
		logger.Log.Error().Err(err).Msg("failed to publish state update")
	}
}

// handleRead transitions each message to READ and notifies the original sender.
func (c *Client) handleRead(ctx context.Context, raw json.RawMessage) {
	var p ReadPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		logger.Log.Error().Err(err).Msg("bad read payload")
		return
	}

	for _, msgIDStr := range p.MessageIDs {
		newState, err := c.stateService.Transition(ctx, msgIDStr, service.StateRead)
		if err != nil && !errors.Is(err, service.ErrStaleTransition) {
			logger.Log.Error().Err(err).Str("message_id", msgIDStr).Msg("read transition failed")
			continue
		}

		msgUUID, err := uuid.Parse(msgIDStr)
		if err != nil {
			logger.Log.Error().Err(err).Str("message_id", msgIDStr).Msg("invalid message UUID in read")
			continue
		}

		msg, err := c.msgRepo.GetByID(ctx, msgUUID)
		if err != nil {
			logger.Log.Error().Err(err).Str("message_id", msgIDStr).Msg("failed to fetch message for read")
			continue
		}

		outbound, _ := json.Marshal(Envelope{
			Type: "state_update",
			Payload: mustMarshal(StateUpdatePayload{
				MessageID: msgIDStr,
				State:     newState,
			}),
		})
		if err := c.pubsubRepo.Publish(ctx, msg.SenderID.String(), outbound); err != nil {
			logger.Log.Error().Err(err).Str("message_id", msgIDStr).Msg("failed to publish read state update")
		}
	}
}

func (c *Client) sendJSON(msgType string, payload any) {
	env := Envelope{
		Type:    msgType,
		Payload: mustMarshal(payload),
	}
	out, _ := json.Marshal(env)
	select {
	case c.send <- out:
	default:
		logger.Log.Warn().Str("userID", c.userID).Msg("send channel full, dropping frame")
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
		logger.Log.Debug().Str("userID", c.userID).Msg("writePump exited")
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			for range len(c.send) {
				w.Write([]byte("\n"))
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("marshal failed: " + err.Error())
	}
	return data
}

