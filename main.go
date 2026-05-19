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

	cache, err := translator.NewCache("translations.db")
	if err != nil {
		logger.Fatal("opening translation cache", zap.Error(err))
	}
	defer cache.Close()

	trans := translator.NewTranslator(cfg.DeepL.APIKey, cfg.DeepL.BaseURL, cache)

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

		// Auto-create destination group if not configured
		if cfg.Channels.Destination == 0 {
			destID, err := createDestinationGroup(ctx, api, cfg, trans, logger)
			if err != nil {
				return fmt.Errorf("creating destination group: %w", err)
			}
			cfg.Channels.Destination = destID
			logger.Info("auto-created destination group", zap.Int64("id", destID))
		} else if *cfg.Channels.OverrideGroupName {
			// Rename existing destination group
			if err := renameDestinationGroup(ctx, api, cfg, logger); err != nil {
				logger.Warn("failed to rename destination group", zap.Error(err))
			}
		}

		// Resolve channel peers
		if err := resolveChannels(ctx, api, cfg, b, logger); err != nil {
			return fmt.Errorf("resolving channels: %w", err)
		}

		// Sync group photo with flag overlay
		if err := b.SyncGroupPhoto(ctx, api); err != nil {
			logger.Warn("syncing group photo", zap.Error(err))
		}

		// Sync forum topics: enable forum on destination, create mirror topics
		if err := b.SyncForumTopics(ctx, api); err != nil {
			logger.Error("syncing forum topics", zap.Error(err))
		}

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

var langFlags = map[string]string{
	"AR": "\U0001F1E6\U0001F1EA", // 🇦🇪
	"BG": "\U0001F1E7\U0001F1EC", // 🇧🇬
	"CS": "\U0001F1E8\U0001F1FF", // 🇨🇿
	"DA": "\U0001F1E9\U0001F1F0", // 🇩🇰
	"DE": "\U0001F1E9\U0001F1EA", // 🇩🇪
	"EL": "\U0001F1EC\U0001F1F7", // 🇬🇷
	"EN": "\U0001F1EC\U0001F1E7", // 🇬🇧
	"ES": "\U0001F1EA\U0001F1F8", // 🇪🇸
	"ET": "\U0001F1EA\U0001F1EA", // 🇪🇪
	"FI": "\U0001F1EB\U0001F1EE", // 🇫🇮
	"FR": "\U0001F1EB\U0001F1F7", // 🇫🇷
	"HU": "\U0001F1ED\U0001F1FA", // 🇭🇺
	"ID": "\U0001F1EE\U0001F1E9", // 🇮🇩
	"IT": "\U0001F1EE\U0001F1F9", // 🇮🇹
	"JA": "\U0001F1EF\U0001F1F5", // 🇯🇵
	"KO": "\U0001F1F0\U0001F1F7", // 🇰🇷
	"LT": "\U0001F1F1\U0001F1F9", // 🇱🇹
	"LV": "\U0001F1F1\U0001F1FB", // 🇱🇻
	"NB": "\U0001F1F3\U0001F1F4", // 🇳🇴
	"NL": "\U0001F1F3\U0001F1F1", // 🇳🇱
	"PL": "\U0001F1F5\U0001F1F1", // 🇵🇱
	"PT": "\U0001F1F5\U0001F1F9", // 🇵🇹
	"RO": "\U0001F1F7\U0001F1F4", // 🇷🇴
	"RU": "\U0001F1F7\U0001F1FA", // 🇷🇺
	"SK": "\U0001F1F8\U0001F1F0", // 🇸🇰
	"SL": "\U0001F1F8\U0001F1EE", // 🇸🇮
	"SV": "\U0001F1F8\U0001F1EA", // 🇸🇪
	"TR": "\U0001F1F9\U0001F1F7", // 🇹🇷
	"UK": "\U0001F1FA\U0001F1E6", // 🇺🇦
	"ZH": "\U0001F1E8\U0001F1F3", // 🇨🇳
}

func createDestinationGroup(ctx context.Context, api *tg.Client, cfg *config.Config, trans *translator.Translator, logger *zap.Logger) (int64, error) {
	// Get source channel name
	srcChannelID := cfg.Channels.Sources[0]

	// Fetch dialogs to find the source channel name
	dialogs, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      100,
	})
	if err != nil {
		return 0, fmt.Errorf("getting dialogs: %w", err)
	}

	var srcName string
	var chats []tg.ChatClass
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		chats = d.Chats
	case *tg.MessagesDialogsSlice:
		chats = d.Chats
	}
	for _, chat := range chats {
		if ch, ok := chat.(*tg.Channel); ok && ch.ID == srcChannelID {
			srcName = ch.Title
			break
		}
	}
	if srcName == "" {
		srcName = fmt.Sprintf("Channel %d", srcChannelID)
	}

	// Build destination group name: "Source Name 🇫🇷"
	flag := langFlags[cfg.Languages.TargetLang]
	groupTitle := fmt.Sprintf("%s %s", srcName, flag)

	logger.Info("creating destination group", zap.String("title", groupTitle))

	updates, err := api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
		Megagroup: true,
		Forum:     true,
		Title:     groupTitle,
		About:     fmt.Sprintf("Auto-translated from %s", srcName),
	})
	if err != nil {
		return 0, fmt.Errorf("creating channel: %w", err)
	}

	// Extract created channel ID
	switch u := updates.(type) {
	case *tg.Updates:
		for _, chat := range u.Chats {
			if ch, ok := chat.(*tg.Channel); ok {
				return ch.ID, nil
			}
		}
	}

	return 0, fmt.Errorf("could not extract channel ID from creation response")
}

func renameDestinationGroup(ctx context.Context, api *tg.Client, cfg *config.Config, logger *zap.Logger) error {
	dialogs, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      100,
	})
	if err != nil {
		return fmt.Errorf("getting dialogs: %w", err)
	}

	var srcName string
	var dstAccessHash int64
	var dstCurrentTitle string
	var chats []tg.ChatClass
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		chats = d.Chats
	case *tg.MessagesDialogsSlice:
		chats = d.Chats
	}
	for _, chat := range chats {
		if ch, ok := chat.(*tg.Channel); ok {
			if ch.ID == cfg.Channels.Sources[0] {
				srcName = ch.Title
			}
			if ch.ID == cfg.Channels.Destination {
				dstAccessHash = ch.AccessHash
				dstCurrentTitle = ch.Title
			}
		}
	}
	if srcName == "" {
		return nil
	}

	flag := langFlags[cfg.Languages.TargetLang]
	newTitle := fmt.Sprintf("%s %s", srcName, flag)

	if dstCurrentTitle == newTitle {
		return nil
	}

	_, err = api.ChannelsEditTitle(ctx, &tg.ChannelsEditTitleRequest{
		Channel: &tg.InputChannel{
			ChannelID:  cfg.Channels.Destination,
			AccessHash: dstAccessHash,
		},
		Title: newTitle,
	})
	if err != nil {
		return fmt.Errorf("editing title: %w", err)
	}

	logger.Info("renamed destination group", zap.String("title", newTitle))
	return nil
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
