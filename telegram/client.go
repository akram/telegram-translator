package telegram

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"go.uber.org/zap"

	"github.com/akram/telegram-translator/config"
)

func NewClient(cfg *config.Config, handler telegram.UpdateHandler, logger *zap.Logger) *telegram.Client {
	return telegram.NewClient(cfg.Telegram.AppID, cfg.Telegram.AppHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.Telegram.SessionFile},
		UpdateHandler:  handler,
		Logger:         logger,
	})
}

func Authenticate(ctx context.Context, client *telegram.Client, phone string) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("getting auth status: %w", err)
	}
	if status.Authorized {
		return nil
	}

	codePrompt := auth.CodeAuthenticatorFunc(func(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
		fmt.Print("Enter verification code: ")
		reader := bufio.NewReader(os.Stdin)
		code, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(code), nil
	})

	flow := auth.NewFlow(
		auth.CodeOnly(phone, codePrompt),
		auth.SendCodeOptions{},
	)

	return flow.Run(ctx, client.Auth())
}
