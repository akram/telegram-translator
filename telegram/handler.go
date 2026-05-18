package telegram

import (
	"context"

	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

type MessageHandler interface {
	HandleSourceMessage(ctx context.Context, api *tg.Client, msg *tg.Message, channelID int64) error
	HandleDestinationMessage(ctx context.Context, api *tg.Client, msg *tg.Message, channelID int64) error
	HandleEditedSourceMessage(ctx context.Context, api *tg.Client, msg *tg.Message, channelID int64) error
	CacheUsersFromEntities(e tg.Entities)
}

type Handler struct {
	sources     map[int64]bool
	destination int64
	bot         MessageHandler
	api         *tg.Client
	logger      *zap.Logger
}

func NewHandler(sources []int64, destination int64, bot MessageHandler, api *tg.Client, logger *zap.Logger) *Handler {
	srcMap := make(map[int64]bool, len(sources))
	for _, id := range sources {
		srcMap[id] = true
	}
	return &Handler{
		sources:     srcMap,
		destination: destination,
		bot:         bot,
		api:         api,
		logger:      logger,
	}
}

func (h *Handler) RegisterOn(dispatcher tg.UpdateDispatcher) {
	dispatcher.OnNewChannelMessage(h.onNewChannelMessage)
	dispatcher.OnEditChannelMessage(h.onEditChannelMessage)
}

func (h *Handler) onNewChannelMessage(ctx context.Context, e tg.Entities, update *tg.UpdateNewChannelMessage) error {
	msg, ok := update.Message.(*tg.Message)
	if !ok {
		return nil // skip service messages
	}

	channelID := extractChannelID(msg)
	if channelID == 0 {
		return nil
	}

	// Cache user info from update entities
	h.bot.CacheUsersFromEntities(e)

	if h.sources[channelID] {
		h.logger.Debug("source channel message",
			zap.Int64("channel", channelID),
			zap.Int("msg_id", msg.ID),
		)
		if err := h.bot.HandleSourceMessage(ctx, h.api, msg, channelID); err != nil {
			h.logger.Error("handling source message", zap.Error(err))
		}
		return nil
	}

	if channelID == h.destination {
		if !msg.Out {
			return nil // ignore messages from others in destination
		}
		h.logger.Debug("destination channel message",
			zap.Int64("channel", channelID),
			zap.Int("msg_id", msg.ID),
		)
		if err := h.bot.HandleDestinationMessage(ctx, h.api, msg, channelID); err != nil {
			h.logger.Error("handling destination message", zap.Error(err))
		}
		return nil
	}

	return nil
}

func (h *Handler) onEditChannelMessage(ctx context.Context, e tg.Entities, update *tg.UpdateEditChannelMessage) error {
	msg, ok := update.Message.(*tg.Message)
	if !ok {
		return nil
	}

	channelID := extractChannelID(msg)
	if channelID == 0 {
		return nil
	}

	if h.sources[channelID] {
		h.logger.Debug("edited source message",
			zap.Int64("channel", channelID),
			zap.Int("msg_id", msg.ID),
		)
		if err := h.bot.HandleEditedSourceMessage(ctx, h.api, msg, channelID); err != nil {
			h.logger.Error("handling edited message", zap.Error(err))
		}
	}

	return nil
}

func extractChannelID(msg *tg.Message) int64 {
	if peer, ok := msg.PeerID.(*tg.PeerChannel); ok {
		return peer.ChannelID
	}
	return 0
}
