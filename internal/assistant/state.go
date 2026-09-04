package assistant

import (
	"context"
	"strconv"

	"github.com/kidandcat/takan/internal/store"
)

// ChatState tracks one chat's agent conversation. It is the in-memory shape of
// an assistant_chats row.
type ChatState struct {
	ConversationStarted bool
	SessionID           string
	ForkFrom            string
	Runs                int64
}

// StateStore is the assistant's persistent state: the Telegram update offset,
// the per-chat session bookkeeping, and the registered push devices. It is
// named StateStore rather than Store so it does not collide with store.Store.
type StateStore struct {
	st     *store.Store
	userID string
	ctx    context.Context
}

// NewStateStore binds the assistant's state to one owner.
func NewStateStore(ctx context.Context, st *store.Store, userID string) *StateStore {
	return &StateStore{st: st, userID: userID, ctx: ctx}
}

func chatKey(chatID int64) string { return strconv.FormatInt(chatID, 10) }

// Offset returns the next Telegram update offset to request.
func (s *StateStore) Offset() int64 {
	raw, err := s.st.AssistantMeta(s.ctx, s.userID, store.MetaTelegramOffset)
	if err != nil || raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// SetOffset records the next update offset.
func (s *StateStore) SetOffset(offset int64) error {
	return s.st.SetAssistantMeta(s.ctx, s.userID, store.MetaTelegramOffset,
		strconv.FormatInt(offset, 10))
}

// BotUsername is the @handle recorded at the last successful getMe, so the
// panel can name the bot even while polling is broken.
func (s *StateStore) BotUsername() string {
	name, err := s.st.AssistantMeta(s.ctx, s.userID, store.MetaBotUsername)
	if err != nil {
		return ""
	}
	return name
}

// SetBotUsername records the @handle after getMe.
func (s *StateStore) SetBotUsername(name string) error {
	return s.st.SetAssistantMeta(s.ctx, s.userID, store.MetaBotUsername, name)
}

// Chat returns a chat's session state, empty when unknown.
func (s *StateStore) Chat(chatID int64) ChatState {
	c, err := s.st.AssistantChatState(s.ctx, s.userID, chatKey(chatID))
	if err != nil {
		return ChatState{}
	}
	return ChatState{
		ConversationStarted: c.ConversationStarted,
		SessionID:           c.SessionID,
		ForkFrom:            c.ForkFrom,
		Runs:                c.Runs,
	}
}

// See records a chat the first time the assistant serves it, so the panel can
// list it without an approval step.
func (s *StateStore) See(chatID int64, kind, title string) error {
	return s.st.SeeAssistantChat(s.ctx, s.userID, chatKey(chatID), kind, title)
}

// MarkConversationStarted records that the chat now resumes sessionID.
func (s *StateStore) MarkConversationStarted(chatID int64, sessionID string) error {
	return s.st.MarkConversationStarted(s.ctx, s.userID, chatKey(chatID), sessionID)
}

// MarkPromoted records that sessionID now belongs to a background task, so the
// next conversational turn forks from it rather than resuming it.
func (s *StateStore) MarkPromoted(chatID int64, sessionID string) error {
	return s.st.MarkConversationPromoted(s.ctx, s.userID, chatKey(chatID), sessionID)
}

// ResetConversation rotates the session: the next run starts a fresh one.
func (s *StateStore) ResetConversation(chatID int64) error {
	return s.st.ResetAssistantConversation(s.ctx, s.userID, chatKey(chatID))
}

// RegisterPushToken records a device token, refreshing one already known.
func (s *StateStore) RegisterPushToken(token, platform string) error {
	return s.st.RegisterPushDevice(s.ctx, s.userID, token, platform)
}

// PushTokens returns every registered device token.
func (s *StateStore) PushTokens() []string {
	devices, err := s.st.ListPushDevices(s.ctx, s.userID)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(devices))
	for _, d := range devices {
		out = append(out, d.Token)
	}
	return out
}

// DeletePushToken forgets a device, used when FCM reports it is gone.
func (s *StateStore) DeletePushToken(token string) error {
	return s.st.DeletePushDevice(s.ctx, s.userID, token)
}
