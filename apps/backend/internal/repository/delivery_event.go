package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DeliveryEvent struct {
	ID        string
	MessageID string
	State     int
}

type DeliveryEventRepo struct {
	db *pgxpool.Pool
}

func NewDeliveryEventRepo(db *pgxpool.Pool) *DeliveryEventRepo {
	return &DeliveryEventRepo{db: db}
}

// InsertEvent records a state transition in the audit trail.
// This is called inside StateService.Transition within the same transaction,
// but is also exposed here for cases where you need manual insertion.
func (r *DeliveryEventRepo) InsertEvent(ctx context.Context, messageID string, state int) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO delivery_events (message_id, state) VALUES ($1, $2)`,
		messageID, state,
	)
	return err
}