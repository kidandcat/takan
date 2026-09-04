package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/kidandcat/takan/internal/store"
)

// channelView renders one Telegram channel: a bot credential plus the chats it
// serves, and everything currently attached to it.
type channelView struct {
	ID, Name, BotUser, BotName string
	IsDefault                  bool
	Chats                      []channelChatView
	Attachments                []attachmentView
	// Receiver names the consumer holding the single getUpdates slot, if any.
	Receiver string
}

type channelChatView struct {
	ChatID, Type, Label string
	Group               bool
}

type attachmentView struct {
	Consumer, ConsumerID, Direction, ChatID, Label string
	Receive                                        bool
}

// fillChannels loads the Telegram channel registry for the panel.
func (s *Server) fillChannels(ctx context.Context, u *store.User, data *pageData) {
	list, err := s.Store.ListTelegramChannels(ctx, u.ID)
	if err != nil {
		return
	}
	botNames := map[string]string{}
	if bots, err := s.Store.ListBots(ctx, u.ID); err == nil {
		for _, b := range bots {
			botNames[b.ID] = b.Name
		}
	}
	for _, c := range list {
		cv := channelView{
			ID: c.ID, Name: c.Name, BotUser: c.BotUser, BotName: c.BotName, IsDefault: c.IsDefault,
		}
		for _, ch := range c.Chats {
			cv.Chats = append(cv.Chats, channelChatView{
				ChatID: ch.ChatID, Type: ch.Type, Label: ch.Display(),
				Group:  ch.Type == store.TelegramChatGroup,
			})
		}
		for _, a := range c.Attachments {
			av := attachmentView{
				Consumer: a.Consumer, ConsumerID: a.ConsumerID, Direction: a.Direction,
				ChatID: a.ChatID, Receive: a.ReceiveConsumer(),
			}
			av.Label = a.Consumer
			if n := botNames[a.ConsumerID]; n != "" {
				av.Label = a.Consumer + " " + n
			}
			if av.Receive {
				cv.Receiver = av.Label
			}
			cv.Attachments = append(cv.Attachments, av)
		}
		data.Channels = append(data.Channels, cv)
	}
	// The bots page offers channels that are free to receive.
	for _, c := range data.Channels {
		if c.Receiver == "" {
			data.BotChannels = append(data.BotChannels, c)
		}
	}
}

func (s *Server) redirectChannels(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/dashboard/telegram?flash="+urlQuery(msg), http.StatusFound)
}

// createChannel validates a BotFather token with getMe and stores it sealed.
func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if s.Telegram == nil {
		s.redirectChannels(w, r, "error: telegram service unavailable")
		return
	}
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	token := strings.TrimSpace(r.FormValue("token"))
	c, err := s.Telegram.AddChannel(r.Context(), u.ID, name, token)
	if err != nil {
		s.redirectChannels(w, r, "error: "+err.Error())
		return
	}
	// An initial chat is optional: a channel with no chats cannot send yet.
	if chat := strings.TrimSpace(r.FormValue("chat_id")); chat != "" {
		if err := s.Store.AddChannelChat(r.Context(), u.ID, c.ID, chat,
			r.FormValue("chat_type"), r.FormValue("chat_label")); err != nil {
			s.redirectChannels(w, r, "error: channel created but chat rejected: "+err.Error())
			return
		}
	}
	label := c.Name
	if c.BotUser != "" {
		label += " (@" + c.BotUser + ")"
	}
	s.redirectChannels(w, r, "Channel "+label+" added")
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.DeleteTelegramChannel(r.Context(), u.ID, r.PathValue("id")); err != nil {
		s.redirectChannels(w, r, "error: "+err.Error())
		return
	}
	s.redirectChannels(w, r, "Channel removed")
}

func (s *Server) defaultChannel(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.SetDefaultTelegramChannel(r.Context(), u.ID, r.PathValue("id")); err != nil {
		s.redirectChannels(w, r, "error: "+err.Error())
		return
	}
	s.redirectChannels(w, r, "Default channel updated")
}

func (s *Server) addChannelChat(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	err := s.Store.AddChannelChat(r.Context(), u.ID, r.PathValue("id"),
		r.FormValue("chat_id"), r.FormValue("chat_type"), r.FormValue("chat_label"))
	if err != nil {
		s.redirectChannels(w, r, "error: "+err.Error())
		return
	}
	s.redirectChannels(w, r, "Chat added to channel")
}

func (s *Server) removeChannelChat(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.RemoveChannelChat(r.Context(), u.ID, r.PathValue("id"), r.PathValue("chat")); err != nil {
		s.redirectChannels(w, r, "error: "+err.Error())
		return
	}
	s.redirectChannels(w, r, "Chat removed from channel")
}

// discoverChannelChats runs getUpdates so the operator can pick up a group id
// after adding the bot to the group. Refused while a daemon is consuming.
func (s *Server) discoverChannelChats(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if s.Telegram == nil {
		s.redirectChannels(w, r, "error: telegram service unavailable")
		return
	}
	id := r.PathValue("id")
	found, err := s.Telegram.DiscoverChatsFor(r.Context(), u.ID, id)
	if err != nil {
		s.redirectChannels(w, r, "error: "+err.Error())
		return
	}
	if len(found) == 0 {
		s.redirectChannels(w, r, "No chats seen yet — message the bot (or add it to the group and send a message), then retry")
		return
	}
	added := 0
	for _, c := range found {
		label := strings.TrimSpace(c.Title)
		if label == "" {
			label = strings.TrimSpace(c.First + " " + c.Last)
		}
		if err := s.Store.AddChannelChat(r.Context(), u.ID, id, c.ID, c.Type, label); err == nil {
			added++
		}
	}
	s.redirectChannels(w, r, "Discovered "+itoa(added)+" chat(s)")
}

// attachChannel binds a consumer (send direction) from the channel page.
func (s *Server) attachChannel(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	consumer := strings.TrimSpace(r.FormValue("consumer"))
	switch consumer {
	case store.ConsumerNotifier, store.ConsumerEmail:
	default:
		s.redirectChannels(w, r, "error: unknown consumer")
		return
	}
	err := s.Store.AttachChannel(r.Context(), u.ID, store.ChannelAttachment{
		ChannelID: r.PathValue("id"), Consumer: consumer,
		Direction: store.DirectionSend, ChatID: strings.TrimSpace(r.FormValue("chat_id")),
	})
	if err != nil {
		s.redirectChannels(w, r, "error: "+err.Error())
		return
	}
	s.redirectChannels(w, r, consumer+" now sends on this channel")
}

func (s *Server) detachChannel(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	err := s.Store.DetachChannel(r.Context(), u.ID, r.PathValue("id"),
		r.FormValue("consumer"), r.FormValue("consumer_id"), r.FormValue("direction"))
	if err != nil {
		s.redirectChannels(w, r, "error: "+err.Error())
		return
	}
	s.redirectChannels(w, r, "Detached")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
