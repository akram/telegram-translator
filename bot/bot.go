package bot

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
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
	// Topic mapping: source topic ID -> destination topic ID (and reverse)
	topicMap    map[int]int // src topic -> dst topic
	topicMapRev map[int]int // dst topic -> src topic
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
		topicMap:     make(map[int]int),
		topicMapRev:  make(map[int]int),
	}
}

func (b *Bot) SetPeer(channelID int64, peer *tg.InputPeerChannel) {
	b.peerCache[channelID] = peer
}

// SyncForumTopics enables forum mode on the destination if needed, then mirrors
// all source topics into the destination channel.
func (b *Bot) SyncForumTopics(ctx context.Context, api *tg.Client) error {
	for _, channelID := range b.cfg.Channels.Sources {
		srcPeer, err := b.getPeer(channelID)
		if err != nil {
			continue
		}

		// Fetch source topics
		srcResult, err := api.MessagesGetForumTopics(ctx, &tg.MessagesGetForumTopicsRequest{
			Peer:  srcPeer,
			Limit: 100,
		})
		if err != nil {
			b.logger.Debug("channel is not a forum, skipping topic sync",
				zap.Int64("channel", channelID),
				zap.Error(err),
			)
			continue
		}

		// Collect source topics
		var srcTopics []*tg.ForumTopic
		for _, t := range srcResult.Topics {
			if ft, ok := t.(*tg.ForumTopic); ok {
				srcTopics = append(srcTopics, ft)
			}
		}
		if len(srcTopics) == 0 {
			continue
		}

		// Pick default topic (first open one)
		for _, ft := range srcTopics {
			if !ft.Closed && !ft.Hidden {
				b.defaultTopic[channelID] = ft.ID
				break
			}
		}

		// Enable forum on destination if needed
		dstPeer, err := b.getPeer(b.cfg.Channels.Destination)
		if err != nil {
			return err
		}

		if err := b.enableForum(ctx, api, dstPeer); err != nil {
			return fmt.Errorf("enabling forum on destination: %w", err)
		}

		// Fetch existing destination topics
		dstResult, err := api.MessagesGetForumTopics(ctx, &tg.MessagesGetForumTopicsRequest{
			Peer:  dstPeer,
			Limit: 100,
		})
		if err != nil {
			return fmt.Errorf("fetching destination topics: %w", err)
		}

		dstByTitle := make(map[string]int)
		for _, t := range dstResult.Topics {
			if ft, ok := t.(*tg.ForumTopic); ok {
				dstByTitle[ft.Title] = ft.ID
			}
		}

		// Create missing topics in destination
		for _, srcTopic := range srcTopics {
			if srcTopic.Hidden {
				// Map "General" topic (ID 1) to destination's General (ID 1)
				b.topicMap[srcTopic.ID] = 1
				b.topicMapRev[1] = srcTopic.ID
				continue
			}

			// Translate the topic title
			translatedTitle, err := b.translator.Translate(ctx, srcTopic.Title, b.cfg.Languages.SourceLang, b.cfg.Languages.TargetLang)
			if err != nil || translatedTitle == "" {
				translatedTitle = srcTopic.Title // fallback to original
			}

			if dstID, ok := dstByTitle[translatedTitle]; ok {
				// Topic already exists with translated title
				b.topicMap[srcTopic.ID] = dstID
				b.topicMapRev[dstID] = srcTopic.ID
				b.logger.Info("mapped existing topic",
					zap.String("src_title", srcTopic.Title),
					zap.String("dst_title", translatedTitle),
					zap.Int("src", srcTopic.ID),
					zap.Int("dst", dstID),
				)
				continue
			}

			// Check if it exists with the original (untranslated) title — rename it
			if dstID, ok := dstByTitle[srcTopic.Title]; ok {
				b.topicMap[srcTopic.ID] = dstID
				b.topicMapRev[dstID] = srcTopic.ID

				// Rename to translated title
				if translatedTitle != srcTopic.Title {
					editReq := &tg.MessagesEditForumTopicRequest{
						Peer:    dstPeer,
						TopicID: dstID,
					}
					editReq.SetTitle(translatedTitle)
					if _, err := api.MessagesEditForumTopic(ctx, editReq); err != nil {
						b.logger.Error("renaming topic", zap.String("title", srcTopic.Title), zap.Error(err))
					} else {
						b.logger.Info("renamed topic",
							zap.String("from", srcTopic.Title),
							zap.String("to", translatedTitle),
							zap.Int("dst", dstID),
						)
					}
				}
				continue
			}

			// Create topic in destination with translated title
			randID, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
			req := &tg.MessagesCreateForumTopicRequest{
				Peer:     dstPeer,
				Title:    translatedTitle,
				RandomID: randID.Int64(),
			}
			if srcTopic.IconColor != 0 {
				req.SetIconColor(srcTopic.IconColor)
			}
			if srcTopic.IconEmojiID != 0 {
				req.SetIconEmojiID(srcTopic.IconEmojiID)
			}

			updates, err := api.MessagesCreateForumTopic(ctx, req)
			if err != nil {
				// Retry without emoji if Premium is required
				if strings.Contains(err.Error(), "PREMIUM_ACCOUNT_REQUIRED") {
					req.Flags.Unset(3) // clear IconEmojiID flag
					req.IconEmojiID = 0
					updates, err = api.MessagesCreateForumTopic(ctx, req)
				}
				if err != nil {
					b.logger.Error("creating destination topic",
						zap.String("title", srcTopic.Title),
						zap.Error(err),
					)
					continue
				}
			}

			// Extract created topic ID from updates
			dstTopicID := extractTopicIDFromUpdates(updates)
			if dstTopicID != 0 {
				b.topicMap[srcTopic.ID] = dstTopicID
				b.topicMapRev[dstTopicID] = srcTopic.ID
				b.logger.Info("created mirror topic",
					zap.String("src_title", srcTopic.Title),
					zap.String("dst_title", translatedTitle),
					zap.Int("src", srcTopic.ID),
					zap.Int("dst", dstTopicID),
				)
			}
		}

		b.logger.Info("topic sync complete",
			zap.Int64("channel", channelID),
			zap.Int("topics", len(b.topicMap)),
		)
	}
	return nil
}

func (b *Bot) enableForum(ctx context.Context, api *tg.Client, dstPeer tg.InputPeerClass) error {
	// First check if forum mode is already enabled by trying to fetch topics
	_, err := api.MessagesGetForumTopics(ctx, &tg.MessagesGetForumTopicsRequest{
		Peer:  dstPeer,
		Limit: 1,
	})
	if err == nil {
		b.logger.Info("destination already has forum mode enabled")
		return nil
	}

	// Try to enable forum mode
	dstInputPeer := dstPeer.(*tg.InputPeerChannel)
	channel := &tg.InputChannel{ChannelID: dstInputPeer.ChannelID, AccessHash: dstInputPeer.AccessHash}

	_, err = api.ChannelsToggleForum(ctx, &tg.ChannelsToggleForumRequest{
		Channel: channel,
		Enabled: true,
	})
	if err != nil {
		if strings.Contains(err.Error(), "CHAT_NOT_MODIFIED") {
			return nil
		}
		return fmt.Errorf("cannot enable forum mode — make sure destination is a supergroup, not a broadcast channel: %w", err)
	}
	b.logger.Info("enabled forum mode on destination channel")
	return nil
}

func extractTopicIDFromUpdates(updates tg.UpdatesClass) int {
	switch u := updates.(type) {
	case *tg.Updates:
		for _, update := range u.Updates {
			if upd, ok := update.(*tg.UpdateNewChannelMessage); ok {
				if msg, ok := upd.Message.(*tg.MessageService); ok {
					return msg.ID
				}
			}
		}
	}
	return 0
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
	srcTopicID := extractTopMsgID(msg)

	// Resolve destination topic (mirror)
	dstTopicID := b.topicMap[srcTopicID]

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
		fwdIDs, err := b.sender.ForwardMessages(ctx, srcPeer, []int{msg.ID}, dstPeer, dstTopicID)
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

			captionMsgID, err := b.sender.SendTranslation(ctx, dstPeer, formattedMsg, replyTo, dstTopicID)
			if err != nil {
				return fmt.Errorf("sending caption translation: %w", err)
			}
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
			sentID, err := b.sender.SendTranslation(ctx, dstPeer, part, replyTo, dstTopicID)
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
			TopMsgID:     srcTopicID,
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
	var srcTopicID int

	if replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok && replyTo.ReplyToMsgID != 0 {
		mapping, _ := b.store.LookupByDestination(channelID, replyTo.ReplyToMsgID)
		if mapping != nil {
			srcChannelID = mapping.SrcChannelID
			srcReplyTo = mapping.SrcMsgID
			srcTopicID = mapping.TopMsgID
		}
	}

	// Default to first source channel if not replying to a specific message
	if srcChannelID == 0 && len(b.cfg.Channels.Sources) > 0 {
		srcChannelID = b.cfg.Channels.Sources[0]
	}

	// For standalone messages, resolve source topic from destination topic
	if srcTopicID == 0 {
		dstTopicID := extractTopMsgID(msg)
		if dstTopicID != 0 {
			if st, ok := b.topicMapRev[dstTopicID]; ok {
				srcTopicID = st
			}
		}
	}

	// Fallback to default topic
	if srcTopicID == 0 {
		if dt, ok := b.defaultTopic[srcChannelID]; ok {
			srcTopicID = dt
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

	sentID, err := b.sender.SendTranslation(ctx, srcPeer, translated, srcReplyTo, srcTopicID)
	if err != nil {
		if strings.Contains(err.Error(), "TOPIC_CLOSED") {
			b.logger.Warn("topic is closed, cannot reply",
				zap.Int64("channel", srcChannelID),
				zap.Int("topic", srcTopicID),
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
