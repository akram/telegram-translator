package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram  TelegramConfig  `yaml:"telegram"`
	DeepL     DeepLConfig     `yaml:"deepl"`
	Channels  ChannelsConfig  `yaml:"channels"`
	Languages LanguagesConfig `yaml:"languages"`
}

type TelegramConfig struct {
	AppID       int    `yaml:"app_id"`
	AppHash     string `yaml:"app_hash"`
	PhoneNumber string `yaml:"phone_number"`
	SessionFile string `yaml:"session_file"`
}

type DeepLConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
}

type ChannelsConfig struct {
	Sources     []int64 `yaml:"sources"`
	Destination int64   `yaml:"destination"`
}

type LanguagesConfig struct {
	SourceLang string `yaml:"source_lang"`
	TargetLang string `yaml:"target_lang"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyEnvOverrides(&cfg)
	setDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("TELEGRAM_APP_ID"); v != "" {
		if id, err := strconv.Atoi(v); err == nil {
			cfg.Telegram.AppID = id
		}
	}
	if v := os.Getenv("TELEGRAM_APP_HASH"); v != "" {
		cfg.Telegram.AppHash = v
	}
	if v := os.Getenv("TELEGRAM_PHONE"); v != "" {
		cfg.Telegram.PhoneNumber = v
	}
	if v := os.Getenv("DEEPL_API_KEY"); v != "" {
		cfg.DeepL.APIKey = v
	}
	if v := os.Getenv("DEEPL_BASE_URL"); v != "" {
		cfg.DeepL.BaseURL = v
	}
	if v := os.Getenv("CHANNEL_SOURCES"); v != "" {
		var sources []int64
		for _, s := range strings.Split(v, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				sources = append(sources, id)
			}
		}
		if len(sources) > 0 {
			cfg.Channels.Sources = sources
		}
	}
	if v := os.Getenv("CHANNEL_DESTINATION"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Channels.Destination = id
		}
	}
}

func setDefaults(cfg *Config) {
	if cfg.Telegram.SessionFile == "" {
		cfg.Telegram.SessionFile = "session.json"
	}
	if cfg.DeepL.BaseURL == "" {
		cfg.DeepL.BaseURL = "https://api-free.deepl.com"
	}
	if cfg.Languages.SourceLang == "" {
		cfg.Languages.SourceLang = "AR"
	}
	if cfg.Languages.TargetLang == "" {
		cfg.Languages.TargetLang = "FR"
	}
}

func validate(cfg *Config) error {
	if cfg.Telegram.AppID == 0 {
		return fmt.Errorf("telegram.app_id is required")
	}
	if cfg.Telegram.AppHash == "" {
		return fmt.Errorf("telegram.app_hash is required")
	}
	if cfg.Telegram.PhoneNumber == "" {
		return fmt.Errorf("telegram.phone_number is required")
	}
	if cfg.DeepL.APIKey == "" {
		return fmt.Errorf("deepl.api_key is required")
	}
	if len(cfg.Channels.Sources) == 0 {
		return fmt.Errorf("channels.sources must have at least one channel")
	}
	if cfg.Channels.Destination == 0 {
		return fmt.Errorf("channels.destination is required")
	}
	return nil
}
