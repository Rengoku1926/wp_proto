package repository

import (
	"context"
	"errors"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/errs"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type UserRepository struct {
	pool *pgxpool.Pool
}

func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{pool: pool}
}

func (r *UserRepository) Create(ctx context.Context, username string) (*model.User, error){
	user := &model.User{}

	query := `INSERT INTO users (username) VALUES ($1) RETURNING id, username, created_at`

	err := r.pool.QueryRow(ctx, query, username).Scan(&user.ID, &user.Username, &user.CreatedAt)

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
            return nil, errs.ErrDuplicate
        }
        return nil, err
	}

	return user, nil
		
}

func (r *UserRepository) GetById(ctx context.Context, id uuid.UUID) (*model.User, error){
	user := &model.User{}

	query := `SELECT id, username, created_at FROM users WHERE id = $1`

	err := r.pool.QueryRow(ctx, query, id).Scan(&user.ID, &user.Username, &user.CreatedAt)

	if err != nil{
		if errors.Is(err, pgx.ErrNoRows){
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return user, nil
}

func (r *UserRepository) List(ctx context.Context) ([]*model.User, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, username, created_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*model.User
	for rows.Next() {
		u := &model.User{}
		if err := rows.Scan(&u.ID, &u.Username, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (r *UserRepository) GetByUsername(ctx context.Context, username string) (*model.User, error){
	user := &model.User{}

	query := `SELECT id, username, created_at FROM users WHERE username = $1`

	err := r.pool.QueryRow(ctx, query, username).Scan(&user.ID, &user.Username, &user.CreatedAt)

	if err != nil{
		if errors.Is(err, pgx.ErrNoRows){
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return user, nil
}