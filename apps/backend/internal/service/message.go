package service

import (
	"context"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/google/uuid"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

type HistoryResponse struct {
	Messages   []model.Message `json:"messages"`
	NextCursor string          `json:"next_cursor,omitempty"`
	HasMore    bool            `json:"has_more"`
}

type MessageService struct {
	repo *repository.MessageRepo
}

func NewMessageService(repo *repository.MessageRepo) *MessageService {
	return &MessageService{repo: repo}
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultPageSize
	}
	if limit > maxPageSize {
		return maxPageSize
	}
	return limit
}

func (s *MessageService) GetConversationHistory(
	ctx context.Context,
	userID uuid.UUID,
	otherUserID uuid.UUID,
	cursor time.Time,
	limit int,
) (*HistoryResponse, error) {
	limit = clampLimit(limit)

	messages, err := s.repo.GetConversationHistory(ctx, userID, otherUserID, cursor, limit+1)
	if err != nil {
		return nil, err
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	resp := &HistoryResponse{Messages: messages, HasMore: hasMore}
	if hasMore && len(messages) > 0 {
		resp.NextCursor = messages[len(messages)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	return resp, nil
}

func (s *MessageService) GetGroupHistory(
	ctx context.Context,
	groupID uuid.UUID,
	cursor time.Time,
	limit int,
) (*HistoryResponse, error) {
	limit = clampLimit(limit)

	messages, err := s.repo.GetGroupHistory(ctx, groupID, cursor, limit+1)
	if err != nil {
		return nil, err
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	resp := &HistoryResponse{Messages: messages, HasMore: hasMore}
	if hasMore && len(messages) > 0 {
		resp.NextCursor = messages[len(messages)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	return resp, nil
}
