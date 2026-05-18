package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
	"go.uber.org/zap"

	"github.com/akram/telegram-translator/config"
	"github.com/akram/telegram-translator/store"
	tgsender "github.com/akram/telegram-translator/telegram"
	"github.com/akram/telegram-translator/translator"
)

const maxMessageLength = 4096

type Bot struct {
	translator *translator.Translator
	store      *store.Store
	sender     *tgsender.Sender
	cfg        *config.Config
	logger     *zap.Logger

	// Peer cache: channel ID -> InputPeerChannel
	peerCache map[int64]*tg.InputPeerChannel
	// User cache: user ID -> display name
	userCache map[int64]string
	// Default topic ID per source channel (for forum groups)
	defaultTopic map[int64]int
}

func NewBot(
	translator *translator.Translator,
	store *store.Store,
	sender *tgsender.Sender,
	cfg *config.Config,
	logger *zap.Logger,
) *Bot {
	return &Bot{
		translator: translator,
		store:      store,
		sender:     sender,
		cfg:        cfg,
		logger:     logger,
		peerCache:    make(map[int64]*tg.InputPeerChannel),
		userCache:    make(map[int64]string),
		defaultTopic: make(map[int64]int),
	}
}

func (b *Bot) SetPeer(channelID int64, peer *tg.InputPeerChannel) {
	b.peerCache[channelID] = peer
}

// ResolveDefaultTopics fetches forum topics for each source channel and picks the first open one.
func (b *Bot) ResolveDefaultTopics(ctx context.Context, api *tg.Client) {
	for _, channelID := range b.cfg.Channels.Sources {
		peer, err := b.getPeer(channelID)
		if err != nil {
			continue
		}

		result, err := api.MessagesGetForumTopics(ctx, &tg.MessagesGetForumTopicsRequest{
			Peer:  peer,
			Limit: 50,
		})
		if err != nil {
			// Not a forum group — no topics needed
			b.logger.Debug("channel is not a forum or cannot fetch topics",
				zap.Int64("channel", channelID),
				zap.Error(err),
			)
			continue
		}

		for _, topic := range result.Topics {
			ft, ok := topic.(*tg.ForumTopic)
			if !ok || ft.Closed || ft.Hidden {
				continue
			}
			b.defaultTopic[channelID] = ft.ID
			b.logger.Info("default topic resolved",
				zap.Int64("channel", channelID),
				zap.Int("topic_id", ft.ID),
				zap.String("title", ft.Title),
			)
			break
		}

		if _, ok := b.defaultTopic[channelID]; !ok {
			b.logger.Warn("no open topic found for forum channel",
				zap.Int64("channel", channelID),
			)
		}
	}
}

func (b *Bot) CacheUsers(users []tg.UserClass) {
	for _, u := range users {
		user, ok := u.(*tg.User)
		if !ok {
			continue
		}
		name := user.FirstName
		if user.LastName != "" {
			name += " " + user.LastName
		}
		if name == "" && user.Username != "" {
			name = "@" + user.Username
		}
		if name != "" {
			b.userCache[user.ID] = name
		}
	}
}

func (b *Bot) CacheUsersFromEntities(e tg.Entities) {
	for _, user := range e.Users {
		name := user.FirstName
		if user.LastName != "" {
			name += " " + user.LastName
		}
		if name == "" && user.Username != "" {
			name = "@" + user.Username
		}
		if name != "" {
			b.userCache[user.ID] = name
		}
	}
}

func (b *Bot) getPeer(channelID int64) (tg.InputPeerClass, error) {
	peer, ok := b.peerCache[channelID]
	if !ok {
		return nil, fmt.Errorf("peer not found for channel %d", channelID)
	}
	return peer, nil
}

func (b *Bot) FetchHistory(ctx context.Context, api *tg.Client, channelID int64, limit int) error {
	peer, err := b.getPeer(channelID)
	if err != nil {
		return err
	}

	inputPeer := peer.(*tg.InputPeerChannel)

	history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer: inputPeer,
		Limit: limit,
	})
	if err != nil {
		return fmt.Errorf("fetching history for channel %d: %w", channelID, err)
	}

	var messages []tg.MessageClass
	switch h := history.(type) {
	case *tg.MessagesMessages:
		messages = h.Messages
		b.CacheUsers(h.Users)
	case *tg.MessagesMessagesSlice:
		messages = h.Messages
		b.CacheUsers(h.Users)
	case *tg.MessagesChannelMessages:
		messages = h.Messages
		b.CacheUsers(h.Users)
	}

	b.logger.Info("fetched history",
		zap.Int64("channel", channelID),
		zap.Int("count", len(messages)),
	)

	// Process in chronological order (oldest first)
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(*tg.Message)
		if !ok {
			continue
		}

		// Skip if already translated
		if mapping, _ := b.store.LookupBySource(channelID, msg.ID); mapping != nil {
			continue
		}

		if err := b.HandleSourceMessage(ctx, api, msg, channelID); err != nil {
			b.logger.Error("translating history message",
				zap.Int("msg_id", msg.ID),
				zap.Error(err),
			)
		}
	}

	return nil
}

func (b *Bot) HandleSourceMessage(ctx context.Context, api *tg.Client, msg *tg.Message, channelID int64) error {
	text := msg.Message
	hasMedia := msg.Media != nil && !isEmptyMedia(msg.Media)

	if text == "" && !hasMedia {
		return nil
	}

	dstPeer, err := b.getPeer(b.cfg.Channels.Destination)
	if err != nil {
		return err
	}

	srcPeer, err := b.getPeer(channelID)
	if err != nil {
		return err
	}

	// Resolve the author name
	author := b.resolveAuthor(msg)

	// Build permanent link
	link := fmt.Sprintf("https://t.me/c/%d/%d", channelID, msg.ID)

	// Extract forum topic ID from the source message
	topMsgID := extractTopMsgID(msg)

	// Determine reply-to in destination channel (thread replication)
	var dstReplyTo int
	if replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok && replyTo.ReplyToMsgID != 0 {
		if mapping, _ := b.store.LookupBySource(channelID, replyTo.ReplyToMsgID); mapping != nil {
			dstReplyTo = mapping.DstMsgID
		}
	}

	var dstMsgID int

	if hasMedia {
		// Forward the media message first
		fwdIDs, err := b.sender.ForwardMessages(ctx, srcPeer, []int{msg.ID}, dstPeer)
		if err != nil {
			return fmt.Errorf("forwarding media: %w", err)
		}
		if len(fwdIDs) > 0 {
			dstMsgID = fwdIDs[0]
		}

		// If there's text (caption), translate and reply to the forward
		if text != "" {
			translated, err := b.translator.Translate(ctx, text, b.cfg.Languages.SourceLang, b.cfg.Languages.TargetLang)
			if err != nil {
				return fmt.Errorf("translating caption: %w", err)
			}

			formattedMsg := formatTranslation(author, translated, link)
			replyTo := dstMsgID
			if replyTo == 0 {
				replyTo = dstReplyTo
			}

			captionMsgID, err := b.sender.SendTranslation(ctx, dstPeer, formattedMsg, replyTo, 0)
			if err != nil {
				return fmt.Errorf("sending caption translation: %w", err)
			}
			// Use the caption message as the mapping target
			if captionMsgID != 0 {
				dstMsgID = captionMsgID
			}
		}
	} else {
		// Text-only message
		translated, err := b.translator.Translate(ctx, text, b.cfg.Languages.SourceLang, b.cfg.Languages.TargetLang)
		if err != nil {
			return fmt.Errorf("translating message: %w", err)
		}

		formattedMsg := formatTranslation(author, translated, link)

		// Split if needed
		parts := splitMessage(formattedMsg)
		for i, part := range parts {
			replyTo := dstReplyTo
			if i > 0 {
				replyTo = dstMsgID // chain split messages
			}
			sentID, err := b.sender.SendTranslation(ctx, dstPeer, part, replyTo, 0)
			if err != nil {
				return fmt.Errorf("sending translation part %d: %w", i, err)
			}
			if i == 0 {
				dstMsgID = sentID
			}
		}
	}

	// Save mapping
	if dstMsgID != 0 {
		if err := b.store.SaveMapping(store.MessageMapping{
			SrcChannelID: channelID,
			SrcMsgID:     msg.ID,
			DstChannelID: b.cfg.Channels.Destination,
			DstMsgID:     dstMsgID,
			TopMsgID:     topMsgID,
		}); err != nil {
			b.logger.Error("saving mapping", zap.Error(err))
		}
	}

	b.logger.Info("translated source message",
		zap.Int64("src_channel", channelID),
		zap.Int("src_msg", msg.ID),
		zap.Int("dst_msg", dstMsgID),
		zap.String("author", author),
	)

	return nil
}

func (b *Bot) HandleDestinationMessage(ctx context.Context, api *tg.Client, msg *tg.Message, channelID int64) error {
	text := msg.Message
	if text == "" {
		return nil
	}

	// Find which source channel and message to reply to
	var srcChannelID int64
	var srcReplyTo int
	var topMsgID int

	if replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok && replyTo.ReplyToMsgID != 0 {
		mapping, _ := b.store.LookupByDestination(channelID, replyTo.ReplyToMsgID)
		if mapping != nil {
			srcChannelID = mapping.SrcChannelID
			srcReplyTo = mapping.SrcMsgID
			topMsgID = mapping.TopMsgID

			// If topMsgID is missing (old mapping), try to fetch it from the source message
			if topMsgID == 0 && api != nil {
				topMsgID = b.fetchTopMsgID(ctx, api, srcChannelID, srcReplyTo)
			}
		}
	}

	// Default to first source channel if not replying to a specific message
	if srcChannelID == 0 && len(b.cfg.Channels.Sources) > 0 {
		srcChannelID = b.cfg.Channels.Sources[0]
	}

	// Use default topic for standalone messages in forum groups
	if topMsgID == 0 {
		if dt, ok := b.defaultTopic[srcChannelID]; ok {
			topMsgID = dt
		}
	}

	// Translate back: target -> source language
	translated, err := b.translator.Translate(ctx, text, b.cfg.Languages.TargetLang, b.cfg.Languages.SourceLang)
	if err != nil {
		return fmt.Errorf("translating reply: %w", err)
	}

	srcPeer, err := b.getPeer(srcChannelID)
	if err != nil {
		return fmt.Errorf("getting source peer: %w", err)
	}

	sentID, err := b.sender.SendTranslation(ctx, srcPeer, translated, srcReplyTo, topMsgID)
	if err != nil {
		if strings.Contains(err.Error(), "TOPIC_CLOSED") {
			b.logger.Warn("topic is closed, cannot reply",
				zap.Int64("channel", srcChannelID),
				zap.Int("topic", topMsgID),
			)
			return nil
		}
		return fmt.Errorf("sending reply to source: %w", err)
	}

	// Save mapping for the reply
	if sentID != 0 {
		if err := b.store.SaveMapping(store.MessageMapping{
			SrcChannelID: srcChannelID,
			SrcMsgID:     sentID,
			DstChannelID: channelID,
			DstMsgID:     msg.ID,
		}); err != nil {
			b.logger.Error("saving reply mapping", zap.Error(err))
		}
	}

	b.logger.Info("sent reply to source",
		zap.Int64("src_channel", srcChannelID),
		zap.Int("src_msg", sentID),
		zap.Int("reply_to", srcReplyTo),
	)

	return nil
}

func (b *Bot) HandleEditedSourceMessage(ctx context.Context, api *tg.Client, msg *tg.Message, channelID int64) error {
	if msg.Message == "" {
		return nil
	}

	mapping, err := b.store.LookupBySource(channelID, msg.ID)
	if err != nil || mapping == nil {
		return nil // no mapping found, nothing to edit
	}

	translated, err := b.translator.Translate(ctx, msg.Message, b.cfg.Languages.SourceLang, b.cfg.Languages.TargetLang)
	if err != nil {
		return fmt.Errorf("translating edited message: %w", err)
	}

	author := b.resolveAuthor(msg)
	link := fmt.Sprintf("https://t.me/c/%d/%d", channelID, msg.ID)
	formattedMsg := formatTranslation(author, translated, link)

	dstPeer, err := b.getPeer(mapping.DstChannelID)
	if err != nil {
		return err
	}

	if err := b.sender.EditMessage(ctx, dstPeer, mapping.DstMsgID, formattedMsg); err != nil {
		return fmt.Errorf("editing translated message: %w", err)
	}

	b.logger.Info("edited translated message",
		zap.Int64("channel", channelID),
		zap.Int("src_msg", msg.ID),
		zap.Int("dst_msg", mapping.DstMsgID),
	)

	return nil
}

func (b *Bot) resolveAuthor(msg *tg.Message) string {
	if msg.PostAuthor != "" {
		return msg.PostAuthor
	}
	if peer, ok := msg.FromID.(*tg.PeerUser); ok {
		if name, found := b.userCache[peer.UserID]; found {
			return name
		}
		return fmt.Sprintf("User %d", peer.UserID)
	}
	return "Unknown"
}

func formatTranslation(author, text, link string) string {
	return fmt.Sprintf("%s :\n%s\n\n\U0001F517 %s", author, text, link)
}

func splitMessage(text string) []string {
	if len(text) <= maxMessageLength {
		return []string{text}
	}

	var parts []string
	for len(text) > 0 {
		end := maxMessageLength
		if end > len(text) {
			end = len(text)
		}
		// Try to split at a newline or space
		if end < len(text) {
			if idx := strings.LastIndex(text[:end], "\n"); idx > end/2 {
				end = idx + 1
			} else if idx := strings.LastIndex(text[:end], " "); idx > end/2 {
				end = idx + 1
			}
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	return parts
}

func isEmptyMedia(media tg.MessageMediaClass) bool {
	_, ok := media.(*tg.MessageMediaEmpty)
	return ok
}

func (b *Bot) fetchTopMsgID(ctx context.Context, api *tg.Client, channelID int64, msgID int) int {
	peer, err := b.getPeer(channelID)
	if err != nil {
		return 0
	}
	inputPeer := peer.(*tg.InputPeerChannel)
	inputChannel := &tg.InputChannel{ChannelID: inputPeer.ChannelID, AccessHash: inputPeer.AccessHash}

	result, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: inputChannel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		b.logger.Debug("failed to fetch message for topic ID", zap.Error(err))
		return 0
	}

	if msgs, ok := result.(*tg.MessagesChannelMessages); ok && len(msgs.Messages) > 0 {
		if msg, ok := msgs.Messages[0].(*tg.Message); ok {
			topID := extractTopMsgID(msg)
			if topID != 0 {
				b.logger.Debug("resolved topic ID from source message",
					zap.Int("msg_id", msgID),
					zap.Int("topic_id", topID),
				)
			}
			return topID
		}
	}
	return 0
}

func extractTopMsgID(msg *tg.Message) int {
	replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
	if !ok {
		return 0
	}
	// Reply within a topic: ReplyToTopID is the topic's top message ID
	if replyTo.ReplyToTopID != 0 {
		return replyTo.ReplyToTopID
	}
	// Top-level message in a topic: ReplyToMsgID IS the topic ID
	if replyTo.ForumTopic && replyTo.ReplyToMsgID != 0 {
		return replyTo.ReplyToMsgID
	}
	return 0
}
