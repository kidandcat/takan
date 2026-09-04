package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidandcat/takan/internal/cryptox"
	"github.com/kidandcat/takan/internal/store"
)

// Service owns channel operations that need both the database and the sealing
// key: creating a channel validates the BotFather token against Telegram and
// stores it sealed, and sending resolves a consumer's channel attachment.
//
// The clear token exists only inside these calls; it is never returned, logged
// or rendered.
type Service struct {
	Store *store.Store
	Box   *cryptox.Box
}

// AddChannel validates a BotFather token with getMe and stores it sealed.
// Returns the created channel (without the clear token).
func (s *Service) AddChannel(ctx context.Context, userID, name, token string) (*store.TelegramChannel, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("bot token required")
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("channel name required")
	}
	me, err := GetMe(ctx, token)
	if err != nil {
		// Telegram's own message is the useful part; it carries no secret.
		return nil, fmt.Errorf("token rejected by Telegram: %w", err)
	}
	sealed, err := s.Box.Seal(token)
	if err != nil {
		return nil, fmt.Errorf("seal token: %w", err)
	}
	return s.Store.CreateTelegramChannel(ctx, userID, name, sealed, me.Username, me.First)
}

// Token unseals a channel's credential for the callers that must talk to
// Telegram (sending) or hand it to a provision run (bots module).
func (s *Service) Token(ctx context.Context, c *store.TelegramChannel) (string, error) {
	if c == nil || strings.TrimSpace(c.TokenEnc) == "" {
		return "", fmt.Errorf("channel has no stored credential")
	}
	tok, err := s.Box.Open(c.TokenEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt bot token: %w", err)
	}
	return tok, nil
}

// SendVia posts text through a channel. chatID may be empty to use the
// channel's first chat.
func (s *Service) SendVia(ctx context.Context, c *store.TelegramChannel, chatID, text, parseMode string) (int64, error) {
	if c == nil {
		return 0, fmt.Errorf("no telegram channel configured — open panel → Telegram and add one")
	}
	target := c.PrimaryChat(chatID)
	if target == "" {
		return 0, fmt.Errorf("channel %q has no chats — add one in panel → Telegram", c.Name)
	}
	tok, err := s.Token(ctx, c)
	if err != nil {
		return 0, err
	}
	return SendMessage(ctx, tok, target, text, parseMode)
}

// SendAs delivers a message on behalf of a consumer, using that consumer's
// channel attachment (falling back to the default channel when it has none).
func (s *Service) SendAs(ctx context.Context, userID, consumer, consumerID, text string) (int64, error) {
	c, chat, err := s.Store.ChannelForConsumer(ctx, userID, consumer, consumerID, store.DirectionSend)
	if err != nil {
		return 0, fmt.Errorf("no telegram channel available: %w", err)
	}
	return s.SendVia(ctx, c, chat, text, "")
}

// DiscoverChatsFor runs getUpdates against a channel's credential so the
// operator can pick up a group id after adding the bot to the group.
//
// Only safe while nothing is consuming that token: getUpdates is exclusive, so
// it refuses when a receive consumer is attached (it would steal the daemon's
// updates).
func (s *Service) DiscoverChatsFor(ctx context.Context, userID, channelID string) ([]discoveredChat, error) {
	c, err := s.Store.TelegramChannelByID(ctx, userID, channelID)
	if err != nil {
		return nil, fmt.Errorf("unknown channel")
	}
	recv, err := s.Store.ReceiverOf(ctx, userID, channelID)
	if err != nil {
		return nil, err
	}
	if recv != nil {
		return nil, fmt.Errorf("a bot instance is consuming this channel's updates — stop it before discovering chats")
	}
	tok, err := s.Token(ctx, c)
	if err != nil {
		return nil, err
	}
	return DiscoverChats(ctx, tok)
}

// Notifier returns the operator-notification function other modules use.
// It routes through the notifier's channel attachment, so changing where
// Takan's own alerts land is a panel action, not a code change.
func (s *Service) Notifier() func(ctx context.Context, userID, text string) error {
	return func(ctx context.Context, userID, text string) error {
		on, err := s.Store.ModuleEnabled(ctx, userID, "telegram")
		if err != nil {
			return err
		}
		if !on {
			return fmt.Errorf("telegram module is disabled")
		}
		_, err = s.SendAs(ctx, userID, store.ConsumerNotifier, "", text)
		return err
	}
}
