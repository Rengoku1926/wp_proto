package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type DeliveryState int16

const (
	StatePending   DeliveryState = 0 // created but not sent over ws
	StateSent      DeliveryState = 1 // delivered to the recipient's ws conn
	StateDelivered DeliveryState = 2 // recipient acked receipt, the device has recieved
	StateRead      DeliveryState = 3 // recipient read the message
	StateFailed    DeliveryState = 4 // message failed to be sent to the server/ message exceeded the ttl 
)

type Message struct {
	ID uuid.UUID `json:"id"`
	ClientID uuid.UUID `json:"client_id"` // idempotency key set by the sender
	SenderID uuid.UUID `json:"sender_id"`
	RecipientID uuid.UUID `json:"recipient_id"`
	GroupID *uuid.UUID `json:"group_id,omitempty"`
	Body string `json:"body"`
	State DeliveryState `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Envelope is the top-level frame sent over the WebSocket.
// Type tells the handler what kind of payload to expect.
//
//	"message"      -> payload is a Message
//	"ack"          -> payload is an ACK
//	"state_update" -> payload is a StateUpdate
//	"read"         -> payload is a StateUpdate with State=Read
type Envelope struct {
	Type string `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// ACK is sent back to the sender after a message is persisted
type ACK struct {
	MessageId uuid.UUID `json:"message_id"`
}

// StateUpdate is used to move a message through delivery states.
type StateUpdate struct {
	MessageID uuid.UUID `json:"message_id"`
	ClientID uuid.UUID `json:"client_id"`
	State DeliveryState `json:"state"`
}