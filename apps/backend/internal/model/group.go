package model

import (
	"time"

	"github.com/google/uuid"
)

// Group represents a chat group.
type Group struct {
	ID        uuid.UUID `json:"id" db:"id"`
	Name      string    `json:"name" db:"name"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// Member represents a user's membership in a group.
type Member struct {
	GroupID  uuid.UUID `json:"group_id" db:"group_id"`
	UserID   uuid.UUID `json:"user_id" db:"user_id"`
	JoinedAt time.Time `json:"joined_at" db:"joined_at"`
}

// DeliveryState represents one member's delivery state for a group message.
type DeliveryStateGroup struct {
	MessageID uuid.UUID `json:"message_id" db:"message_id"`
	MemberID  uuid.UUID `json:"member_id" db:"member_id"`
	State     int16     `json:"state" db:"state"`
}