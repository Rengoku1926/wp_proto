package service

import (
	"context"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/google/uuid"
)

// GroupService contains business logic for group operations.
type GroupService struct {
	groupRepo *repository.GroupRepo
}

// NewGroupService creates a new GroupService.
func NewGroupService(groupRepo *repository.GroupRepo) *GroupService {
	return &GroupService{groupRepo: groupRepo}
}

// CreateGroup creates a new group.
func (s *GroupService) CreateGroup(ctx context.Context, name string) (*model.Group, error) {
	return s.groupRepo.CreateGroup(ctx, name)
}

// AddMember adds a user to a group.
func (s *GroupService) AddMember(ctx context.Context, groupID, userID uuid.UUID) error {
	return s.groupRepo.AddMember(ctx, groupID, userID)
}

// ListMembers returns all members of a group.
func (s *GroupService) ListMembers(ctx context.Context, groupID uuid.UUID) ([]model.Member, error) {
	return s.groupRepo.GetMembers(ctx, groupID)
}