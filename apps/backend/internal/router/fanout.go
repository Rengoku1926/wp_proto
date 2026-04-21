package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

const (
	pendingTTL = 7 * 24 * time.Hour
)

func pendingKey(messageID string) string {
	return fmt.Sprintf("fanout:pending:%s", messageID)
}

// FanoutEngine handles group message distribution.
type FanoutEngine struct {
	Hub               *handler.Hub
	PubSubRepo        *repository.PubSubRepo
	MsgRepo           *repository.MessageRepo
	GroupRepo         *repository.GroupRepo
	GroupDeliveryRepo *repository.GroupDeliveryRepo
	OfflineStore      *repository.OfflineStore
	RDB               *redis.Client
}

// NewFanoutEngine creates a new FanoutEngine.
func NewFanoutEngine(
	hub *handler.Hub,
	pubsubRepo *repository.PubSubRepo,
	msgRepo *repository.MessageRepo,
	groupRepo *repository.GroupRepo,
	groupDeliveryRepo *repository.GroupDeliveryRepo,
	offlineStore *repository.OfflineStore,
	rdb *redis.Client,
) *FanoutEngine {
	return &FanoutEngine{
		Hub:               hub,
		PubSubRepo:        pubsubRepo,
		MsgRepo:           msgRepo,
		GroupRepo:         groupRepo,
		GroupDeliveryRepo: groupDeliveryRepo,
		OfflineStore:      offlineStore,
		RDB:               rdb,
	}
}

// Fanout distributes a group message to all members except the sender.
func (f *FanoutEngine) Fanout(ctx context.Context, senderID string, messageID string, groupID string, content string) error {
	gid, err := uuid.Parse(groupID)
	if err != nil {
		return fmt.Errorf("invalid group ID: %w", err)
	}

	members, err := f.GroupRepo.GetMembers(ctx, gid)
	if err != nil {
		return fmt.Errorf("failed to get group members: %w", err)
	}

	var recipientIDs []uuid.UUID
	for _, m := range members {
		if m.UserID.String() != senderID {
			recipientIDs = append(recipientIDs, m.UserID)
		}
	}

	if len(recipientIDs) == 0 {
		log.Warn().Str("group", groupID).Msg("no recipients in group")
		return nil
	}

	msgID, err := uuid.Parse(messageID)
	if err != nil {
		return fmt.Errorf("invalid message ID: %w", err)
	}

	if err := f.GroupDeliveryRepo.InsertDeliveryRows(ctx, msgID, recipientIDs); err != nil {
		return fmt.Errorf("failed to insert delivery rows: %w", err)
	}

	pk := pendingKey(messageID)
	if err := f.RDB.Set(ctx, pk, len(recipientIDs), pendingTTL).Err(); err != nil {
		return fmt.Errorf("failed to set pending counter: %w", err)
	}

	msgPayload, _ := json.Marshal(map[string]string{
		"message_id": messageID,
		"sender_id":  senderID,
		"group_id":   groupID,
		"content":    content,
	})
	outgoing, _ := json.Marshal(model.Envelope{
		Type:    "message",
		Payload: json.RawMessage(msgPayload),
	})

	savedMsg, err := f.MsgRepo.GetByID(ctx, msgID)
	if err != nil {
		return fmt.Errorf("failed to fetch saved message for fanout: %w", err)
	}

	var wg sync.WaitGroup
	for _, recipientID := range recipientIDs {
		wg.Add(1)
		go func(rid uuid.UUID) {
			defer wg.Done()
			if f.Hub.IsOnline(rid.String()) {
				if err := f.PubSubRepo.Publish(ctx, rid.String(), outgoing); err != nil {
					log.Warn().Err(err).
						Str("message", messageID).
						Str("recipient", rid.String()).
						Msg("pubsub failed for online member, buffering offline")
					if err := f.OfflineStore.Push(ctx, rid.String(), savedMsg); err != nil {
						log.Error().Err(err).Str("recipient", rid.String()).Msg("failed to buffer group message")
					}
				}
			} else {
				if err := f.OfflineStore.Push(ctx, rid.String(), savedMsg); err != nil {
					log.Error().Err(err).Str("recipient", rid.String()).Msg("failed to buffer offline group message")
				}
			}
		}(recipientID)
	}
	wg.Wait()

	log.Info().
		Str("message", messageID).
		Str("group", groupID).
		Int("recipients", len(recipientIDs)).
		Msg("fanout complete")

	return nil
}

// HandleMemberACK processes a delivery/read ACK from one group member.
func (f *FanoutEngine) HandleMemberACK(ctx context.Context, messageID string, memberID string, senderID string, state int16) error {
	msgID, err := uuid.Parse(messageID)
	if err != nil {
		return fmt.Errorf("invalid message ID: %w", err)
	}
	memID, err := uuid.Parse(memberID)
	if err != nil {
		return fmt.Errorf("invalid member ID: %w", err)
	}

	if err := f.GroupDeliveryRepo.UpdateMemberState(ctx, msgID, memID, state); err != nil {
		return fmt.Errorf("failed to update member state: %w", err)
	}

	pk := pendingKey(messageID)
	remaining, err := f.RDB.Decr(ctx, pk).Result()
	if err != nil {
		return fmt.Errorf("failed to decrement pending counter: %w", err)
	}

	log.Debug().
		Str("message", messageID).
		Str("member", memberID).
		Int64("remaining", remaining).
		Msg("member ACK processed")

	if remaining > 0 {
		return nil
	}

	// All members have ACK'd — compute aggregate from Postgres and notify sender.
	aggState, err := f.GroupDeliveryRepo.ComputeAggregateState(ctx, msgID)
	if err != nil {
		return fmt.Errorf("failed to compute aggregate state: %w", err)
	}

	if err := f.MsgRepo.UpdateState(ctx, msgID, model.DeliveryState(aggState)); err != nil {
		log.Error().Err(err).Msg("failed to update message aggregate state")
	}

	suPayload, _ := json.Marshal(map[string]interface{}{
		"message_id": messageID,
		"state":      int(aggState),
	})
	outgoing, _ := json.Marshal(model.Envelope{
		Type:    "state_update",
		Payload: json.RawMessage(suPayload),
	})
	if err := f.PubSubRepo.Publish(ctx, senderID, outgoing); err != nil {
		log.Error().Err(err).Msg("failed to publish aggregate state to sender")
	}

	// Reset counter for the next state level.
	members, err := f.GroupDeliveryRepo.GetAllMemberStates(ctx, msgID)
	if err != nil {
		return nil
	}
	nextState := aggState + 1
	pending := 0
	for _, m := range members {
		if m.State < nextState {
			pending++
		}
	}
	if pending > 0 {
		f.RDB.Set(ctx, pk, pending, pendingTTL)
	} else {
		f.RDB.Del(ctx, pk)
	}

	log.Info().
		Str("message", messageID).
		Int16("aggregate", aggState).
		Msg("group aggregate state updated")

	return nil
}