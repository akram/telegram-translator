package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gotd/td/tg"
	"go.uber.org/zap"

	"github.com/akram/telegram-translator/bot"
	"github.com/akram/telegram-translator/config"
	"github.com/akram/telegram-translator/store"
	tgclient "github.com/akram/telegram-translator/telegram"
	"github.com/akram/telegram-translator/translator"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	logger, err := zap.NewDevelopment()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		logger.Fatal("loading config", zap.Error(err))
	}

	db, err := store.NewStore("translator.db")
	if err != nil {
		logger.Fatal("opening store", zap.Error(err))
	}
	defer db.Close()

	trans := translator.NewTranslator(cfg.DeepL.APIKey, cfg.DeepL.BaseURL)

	// Log DeepL usage
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	used, limit, err := trans.Usage(ctx)
	if err != nil {
		logger.Warn("failed to get DeepL usage", zap.Error(err))
	} else {
		logger.Info("DeepL usage", zap.Int64("used", used), zap.Int64("limit", limit))
	}

	// Create dispatcher
	dispatcher := tg.NewUpdateDispatcher()

	// Create Telegram client
	client := tgclient.NewClient(cfg, &dispatcher, logger.Named("telegram"))

	if err := client.Run(ctx, func(ctx context.Context) error {
		// Authenticate
		if err := tgclient.Authenticate(ctx, client, cfg.Telegram.PhoneNumber); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}
		logger.Info("authenticated successfully")

		api := client.API()

		// Create sender and bot
		sender := tgclient.NewSender(api)
		b := bot.NewBot(trans, db, sender, cfg, logger.Named("bot"))

		// Resolve channel peers
		if err := resolveChannels(ctx, api, cfg, b, logger); err != nil {
			return fmt.Errorf("resolving channels: %w", err)
		}

		// Resolve default forum topics for source channels
		b.ResolveDefaultTopics(ctx, api)

		// Fetch history (last 100 messages) from each source channel
		for _, srcID := range cfg.Channels.Sources {
			logger.Info("fetching history...", zap.Int64("channel", srcID))
			if err := b.FetchHistory(ctx, api, srcID, 100); err != nil {
				logger.Error("fetching history", zap.Int64("channel", srcID), zap.Error(err))
			}
		}

		// Register handlers for live updates
		handler := tgclient.NewHandler(cfg.Channels.Sources, cfg.Channels.Destination, b, api, logger.Named("handler"))
		handler.RegisterOn(dispatcher)

		logger.Info("bot started, listening for messages...",
			zap.Int64s("sources", cfg.Channels.Sources),
			zap.Int64("destination", cfg.Channels.Destination),
		)

		// Block until context is cancelled
		<-ctx.Done()
		return ctx.Err()
	}); err != nil && ctx.Err() == nil {
		logger.Fatal("client error", zap.Error(err))
	}

	logger.Info("shutdown complete")
}

func resolveChannels(ctx context.Context, api *tg.Client, cfg *config.Config, b *bot.Bot, logger *zap.Logger) error {
	// Get all dialogs to populate access hashes
	allChannels := append([]int64{cfg.Channels.Destination}, cfg.Channels.Sources...)

	// Fetch dialogs to get access hashes for channels
	dialogs, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      100,
	})
	if err != nil {
		return fmt.Errorf("getting dialogs: %w", err)
	}

	// Build access hash map from dialogs
	accessHashes := make(map[int64]int64)
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		for _, chat := range d.Chats {
			if ch, ok := chat.(*tg.Channel); ok {
				accessHashes[ch.ID] = ch.AccessHash
			}
		}
	case *tg.MessagesDialogsSlice:
		for _, chat := range d.Chats {
			if ch, ok := chat.(*tg.Channel); ok {
				accessHashes[ch.ID] = ch.AccessHash
			}
		}
	}

	for _, channelID := range allChannels {
		accessHash, ok := accessHashes[channelID]
		if !ok {
			return fmt.Errorf("channel %d not found in dialogs (are you a member?)", channelID)
		}
		b.SetPeer(channelID, &tg.InputPeerChannel{
			ChannelID:  channelID,
			AccessHash: accessHash,
		})
		logger.Info("resolved channel", zap.Int64("id", channelID), zap.Int64("access_hash", accessHash))
	}

	return nil
}
