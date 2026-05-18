package telegram

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/tg"
)

type Sender struct {
	sender *message.Sender
	api    *tg.Client
}

func NewSender(api *tg.Client) *Sender {
	return &Sender{
		sender: message.NewSender(api),
		api:    api,
	}
}

// SendTranslation sends a text message, optionally as a reply. topMsgID is the forum topic ID (0 to skip).
func (s *Sender) SendTranslation(ctx context.Context, peer tg.InputPeerClass, text string, replyToMsgID int, topMsgID int) (int, error) {
	if replyToMsgID != 0 || topMsgID != 0 {
		// Use raw API for full control over reply parameters (forum topics)
		replyTo := &tg.InputReplyToMessage{
			ReplyToMsgID: replyToMsgID,
		}
		if topMsgID != 0 {
			replyTo.SetTopMsgID(topMsgID)
		}

		randID, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
		updates, err := s.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  text,
			RandomID: randID.Int64(),
			ReplyTo:  replyTo,
		})
		if err != nil {
			return 0, fmt.Errorf("sending translation: %w", err)
		}
		return extractMessageID(updates)
	}

	updates, err := s.sender.To(peer).StyledText(ctx, styling.Plain(text))
	if err != nil {
		return 0, fmt.Errorf("sending translation: %w", err)
	}
	return extractMessageID(updates)
}

func (s *Sender) ForwardMessages(ctx context.Context, from tg.InputPeerClass, msgIDs []int, to tg.InputPeerClass) ([]int, error) {
	updates, err := s.api.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: from,
		ID:       msgIDs,
		ToPeer:   to,
		RandomID: generateRandomIDs(len(msgIDs)),
	})
	if err != nil {
		return nil, fmt.Errorf("forwarding messages: %w", err)
	}

	return extractMessageIDs(updates)
}

func (s *Sender) EditMessage(ctx context.Context, peer tg.InputPeerClass, msgID int, text string) error {
	_, err := s.api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
		Peer:    peer,
		ID:      msgID,
		Message: text,
	})
	return err
}

func extractMessageID(updates tg.UpdatesClass) (int, error) {
	ids, err := extractMessageIDs(updates)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("no message IDs in updates")
	}
	return ids[0], nil
}

func extractMessageIDs(updates tg.UpdatesClass) ([]int, error) {
	var ids []int

	switch u := updates.(type) {
	case *tg.Updates:
		for _, update := range u.Updates {
			switch upd := update.(type) {
			case *tg.UpdateNewMessage:
				if msg, ok := upd.Message.(*tg.Message); ok {
					ids = append(ids, msg.ID)
				}
			case *tg.UpdateNewChannelMessage:
				if msg, ok := upd.Message.(*tg.Message); ok {
					ids = append(ids, msg.ID)
				}
			case *tg.UpdateMessageID:
				ids = append(ids, upd.ID)
			}
		}
	case *tg.UpdateShortSentMessage:
		ids = append(ids, u.ID)
	}

	return ids, nil
}

func generateRandomIDs(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = int64(i) + 1 // simple deterministic IDs
	}
	return ids
}
