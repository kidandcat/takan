package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/kidandcat/takan/internal/store"
	botsmod "github.com/kidandcat/takan/modules/bots"
)

// fillBotsDashboard loads bots + their chats for the Bots page and overview card.
func (s *Server) fillBotsDashboard(ctx context.Context, u *store.User, data *pageData) {
	list, err := s.Store.ListBots(ctx, u.ID)
	if err != nil {
		return
	}
	for _, b := range list {
		bv := botView{
			ID: b.ID, Name: b.Name, Username: b.BotUsername, MachineName: b.MachineName,
			Kind: b.Kind, Version: b.Version, Online: botsmod.Online(b), Legacy: b.Legacy(),
			HasToken: b.HasToken, Pending: b.PendingChats, Approved: b.ApprovedChats,
			Deliveries: b.PendingDeliveries,
		}
		if b.LastSeen != nil {
			bv.LastSeen = b.LastSeen.UTC().Format("2006-01-02 15:04")
		}
		if bv.Online {
			data.BotsOnline++
		}
		data.BotsPending += b.PendingChats
		data.BotsDeliveries += b.PendingDeliveries
		chats, _ := s.Store.ListBotChats(ctx, b.ID, "", "")
		for _, c := range chats {
			cv := botChatViewOf(b, c)
			bv.Chats = append(bv.Chats, cv)
			if c.Status == store.BotChatPending {
				data.BotPendingChats = append(data.BotPendingChats, cv)
			}
		}
		data.Bots = append(data.Bots, bv)
	}
}

func botChatViewOf(b store.Bot, c store.BotChat) botChatView {
	return botChatView{
		BotID: b.ID, BotName: b.Name, ChatID: c.ChatID, Type: c.Type, Label: c.Label(),
		Status: c.Status, Snippet: c.FirstMessage,
		Reported: c.CreatedAt.UTC().Format("2006-01-02 15:04"),
		Pending:  c.Status == store.BotChatPending,
		Approved: c.Status == store.BotChatApproved,
	}
}

func (s *Server) createBot(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	machineID := strings.TrimSpace(r.FormValue("machine_id"))
	_, token, err := s.Store.CreateBot(r.Context(), u.ID, name, machineID)
	if err != nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "bots", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	s.flashBotToken(w, token)
	http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("Bot created — copy the token now"), http.StatusFound)
}

// flashBotToken stashes a freshly issued token for one render of the Bots page
// (same show-once pattern as the machine install command / SIP device token).
func (s *Server) flashBotToken(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "takan_bot_token",
		Value:    base64.RawURLEncoding.EncodeToString([]byte(token)),
		Path:     "/",
		MaxAge:   120,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// issueBotToken (re)issues a daemon token, also promoting a legacy owner
// placeholder into a real bot instance.
func (s *Server) issueBotToken(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	token, err := s.Store.IssueBotToken(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	s.flashBotToken(w, token)
	http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("New token issued — copy it now"), http.StatusFound)
}

func (s *Server) deleteBot(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.Store.DeleteBot(r.Context(), u.ID, r.PathValue("id")); err != nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("Bot removed"), http.StatusFound)
}

func (s *Server) approveBotChat(w http.ResponseWriter, r *http.Request) {
	s.decideBotChat(w, r, store.BotChatApproved)
}

func (s *Server) denyBotChat(w http.ResponseWriter, r *http.Request) {
	s.decideBotChat(w, r, store.BotChatDenied)
}

func (s *Server) decideBotChat(w http.ResponseWriter, r *http.Request, status string) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	botID := r.PathValue("id")
	chatID := r.PathValue("chat")
	if _, err := s.Store.DecideBotChat(r.Context(), u.ID, botID, chatID, status, "panel"); err != nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	s.BotWatch.NotifyChats(botID)
	verb := "approved"
	if status == store.BotChatDenied {
		verb = "denied"
	}
	http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("Chat "+chatID+" "+verb), http.StatusFound)
}

func (s *Server) forgetBotChat(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	botID := r.PathValue("id")
	chatID := r.PathValue("chat")
	if err := s.Store.DeleteBotChat(r.Context(), u.ID, botID, chatID); err != nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	s.BotWatch.NotifyChats(botID)
	http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("Chat "+chatID+" forgotten"), http.StatusFound)
}
