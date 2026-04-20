package router

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/rs/zerolog"
)

// Router decides whether to deliver a message via pub/sub (online)
// or push it to the offline buffer (offline).
type Router struct {
	hub          *handler.Hub
	offlineStore *repository.OfflineStore
	pubsubRepo   *repository.PubSubRepo
	msgRepo      *repository.MessageRepo
	log          zerolog.Logger
}

// NewRouter creates a new Router.
func NewRouter(
	hub *handler.Hub,
	offlineStore *repository.OfflineStore,
	pubsubRepo *repository.PubSubRepo,
	msgRepo *repository.MessageRepo,
	log zerolog.Logger,
) *Router {
	return &Router{
		hub:          hub,
		offlineStore: offlineStore,
		pubsubRepo:   pubsubRepo,
		msgRepo:      msgRepo,
		log:          log,
	}
}

func (r *Router) Route(ctx context.Context, recipientID string, msg *model.Message) error {
	if r.hub.IsOnline(recipientID) {
		// Recipient appears to be online. Try pub/sub delivery.
		payload, err := json.Marshal(map[string]any{
			"type": "message",
			"payload": map[string]string{
				"message_id": msg.ID.String(),
				"sender_id":  msg.SenderID.String(),
				"content":    msg.Body,
			},
		})
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}

		err = r.pubsubRepo.Publish(ctx, recipientID, payload)
		if err != nil {
			r.log.Warn().
				Err(err).
				Str("recipient", recipientID).
				Msg("pub/sub publish failed, falling back to offline buffer")

			return r.offlineStore.Push(ctx, recipientID, msg)
		}

		r.log.Debug().
			Str("recipient", recipientID).
			Str("msg_id", msg.ID.String()).
			Msg("message delivered via pub/sub")

		return nil
	}

	r.log.Debug().
		Str("recipient", recipientID).
		Str("msg_id", msg.ID.String()).
		Msg("recipient offline, buffering message")

	return r.offlineStore.Push(ctx, recipientID, msg)
}