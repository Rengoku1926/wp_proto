package repository

import (
	"context"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GroupRepo handles group and membership operations in Postgres.
type GroupRepo struct {
	pool *pgxpool.Pool
}

// NewGroupRepo creates a new GroupRepo.
func NewGroupRepo(pool *pgxpool.Pool) *GroupRepo {
	return &GroupRepo{pool: pool}
}

//CreateGroup creates a new group and returns it 
func(r *GroupRepo) CreateGroup(ctx context.Context, name string) (*model.Group, error){
	g := &model.Group{}
	query := `INSERT INTO groups (name) VALUES ($1) RETURNING id, name, created_at`
	err := r.pool.QueryRow(ctx, query, name).Scan(&g.ID, &g.Name, &g.CreatedAt)
	if err != nil{
		return nil, err
	}
	return g, nil
}

//AddMember adds a user to a group
func (r *GroupRepo) AddMember(ctx context.Context, groupID, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id)
		 VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`,
		groupID, userID,
	)
	return err
}

//RemoveMember removes a user from the group
func(r *GroupRepo) RemoveMember(ctx context.Context, groupID, userID uuid.UUID) error {
	query := `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`
	_, err := r.pool.Exec(ctx, query, groupID, userID)
	return err
}

// GetMembers returns all members of a group.
func (r *GroupRepo) GetMembers(ctx context.Context, groupID uuid.UUID) ([]model.Member, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT group_id, user_id, joined_at
		 FROM group_members
		 WHERE group_id = $1`,
		groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []model.Member
	for rows.Next() {
		var m model.Member
		if err := rows.Scan(&m.GroupID, &m.UserID, &m.JoinedAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// GetGroupsForUser returns all groups a user belongs to.
func (r *GroupRepo) GetGroupsForUser(ctx context.Context, userID uuid.UUID) ([]model.Group, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT g.id, g.name, g.created_at
		 FROM groups g
		 JOIN group_members gm ON g.id = gm.group_id
		 WHERE gm.user_id = $1
		 ORDER BY g.created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []model.Group
	for rows.Next() {
		var g model.Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}