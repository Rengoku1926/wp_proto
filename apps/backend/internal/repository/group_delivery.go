package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GroupDeliveryRepo tracks per-member delivery state for group messages.
type GroupDeliveryRepo struct {
	pool *pgxpool.Pool
}

// NewGroupDeliveryRepo creates a new GroupDeliveryRepo.
func NewGroupDeliveryRepo(pool *pgxpool.Pool) *GroupDeliveryRepo {
	return &GroupDeliveryRepo{pool: pool}
}

// InsertDeliveryRows creates one delivery row per member in a single bulk INSERT.
// All rows start with state=1 (SENT).
func (r *GroupDeliveryRepo) InsertDeliveryRows(ctx context.Context, messageID uuid.UUID, memberIDs []uuid.UUID) error {
	if len(memberIDs) == 0 {
		return nil
	}

	// Build a bulk INSERT: INSERT INTO ... VALUES ($1,$2), ($1,$3), ($1,$4), ...
	// $1 is always the messageID, $2..$N+1 are the memberIDs.
	valueStrings := make([]string, len(memberIDs))
	args := make([]interface{}, 0, 1+len(memberIDs))
	args = append(args, messageID) // $1

	for i, memberID := range memberIDs {
		paramIdx := i + 2 // $2, $3, $4, ...
		valueStrings[i] = fmt.Sprintf("($1, $%d, 1)", paramIdx)
		args = append(args, memberID)
	}

	query := fmt.Sprintf(
		`INSERT INTO group_message_delivery (message_id, member_id, state)
		 VALUES %s
		 ON CONFLICT DO NOTHING`,
		strings.Join(valueStrings, ", "),
	)

	_, err := r.pool.Exec(ctx, query, args...)
	return err
}

// UpdateMemberState updates the delivery state for a specific member.
// State can only move forward (higher value), never backward.
func (r *GroupDeliveryRepo) UpdateMemberState(ctx context.Context, messageID, memberID uuid.UUID, state int16) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE group_message_delivery
		 SET state = $3
		 WHERE message_id = $1 AND member_id = $2 AND state < $3`,
		messageID, memberID, state,
	)
	return err
}

// GetAllMemberStates returns the delivery state for every member of a message.
func (r *GroupDeliveryRepo) GetAllMemberStates(ctx context.Context, messageID uuid.UUID) ([]model.DeliveryStateGroup, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT message_id, member_id, state
		 FROM group_message_delivery
		 WHERE message_id = $1`,
		messageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []model.DeliveryStateGroup
	for rows.Next() {
		var ds model.DeliveryStateGroup
		if err := rows.Scan(&ds.MessageID, &ds.MemberID, &ds.State); err != nil {
			return nil, err
		}
		states = append(states, ds)
	}
	return states, rows.Err()
}

// ComputeAggregateState returns the minimum state across all members for a message.
// This is the state the sender should see.
func (r *GroupDeliveryRepo) ComputeAggregateState(ctx context.Context, messageID uuid.UUID) (int16, error) {
	var minState int16
	err := r.pool.QueryRow(ctx,
		`SELECT COALESCE(MIN(state), 1)
		 FROM group_message_delivery
		 WHERE message_id = $1`,
		messageID,
	).Scan(&minState)
	return minState, err
}