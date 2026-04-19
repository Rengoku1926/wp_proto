package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatePending   = 0
	StateSent      = 1
	StateDelivered = 2
	StateRead      = 3
)

var ErrStaleTransition = errors.New("state transition rejected: new state is not ahead of current state")

type StateService struct {
	db *pgxpool.Pool
}

func NewStateService(db *pgxpool.Pool) *StateService {
	return &StateService{db: db}
}

// Transition moves a message to toState only if toState > current state.
// Updates messages.state and inserts a delivery_events row in one transaction.
func (s *StateService) Transition(ctx context.Context, msgID string, toState int) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var currentState int
	err = tx.QueryRow(ctx,
		`SELECT state FROM messages WHERE id = $1 FOR UPDATE`,
		msgID,
	).Scan(&currentState)
	if err != nil {
		return 0, err
	}

	if toState <= currentState {
		return currentState, ErrStaleTransition
	}

	_, err = tx.Exec(ctx,
		`UPDATE messages SET state = $1, updated_at = now() WHERE id = $2`,
		toState, msgID,
	)
	if err != nil {
		return 0, err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO delivery_events (message_id, state) VALUES ($1, $2)`,
		msgID, toState,
	)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	return toState, nil
}
