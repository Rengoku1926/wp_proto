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