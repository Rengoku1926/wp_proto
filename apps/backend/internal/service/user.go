package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/errs"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

type UserService struct {
	repo *repository.UserRepository
	logger zerolog.Logger
}

func NewUserService(repo *repository.UserRepository, logger zerolog.Logger) *UserService{
	return &UserService{
		repo: repo,
		logger: logger,
	}
}

func (s *UserService) RegisterUser(ctx context.Context, username string)(*model.User, error){
	// Trim whitespace
    username = strings.TrimSpace(username)

    // Validation rules
    if username == "" {
        return nil, fmt.Errorf("%w: username is required", errs.ErrValidation)
    }
    if len(username) < 3 {
        return nil, fmt.Errorf("%w: username must be at least 3 characters", errs.ErrValidation)
    }
    if len(username) > 30 {
        return nil, fmt.Errorf("%w: username must be at most 30 characters", errs.ErrValidation)
    }

	user, err := s.repo.Create(ctx, username)
	if err != nil {
		s.logger.Error().Err(err).Str("username", username).Msg("Failed to create users")
		return nil, err
	}

	s.logger.Info().Str("user_id", user.ID.String()).Str("username", username).Msg("User Registered")
	return user, nil
}

func (s *UserService) GetUser(ctx context.Context, id uuid.UUID) (*model.User, error){
	user, err := s.repo.GetById(ctx, id)
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (s *UserService) GetUserByUsername(ctx context.Context, username string) (*model.User, error){
	user, err := s.repo.GetByUsername(ctx, username)
	if err != nil{
		return nil, err
	}
	return user, nil
}