package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/kidandcat/takan/internal/store"
	botsmod "github.com/kidandcat/takan/modules/bots"
)

// runtimeBundleImportHint is the command that captures a bundle on the hub
// host. It is shown, never run: the panel deliberately has no upload form, so
// the only way in is an operator with shell on the hub.
const runtimeBundleImportHint = `takan bundle import \
  --grok-home /home/debian/.grok \
  --atlas-data /home/debian/atlas-data \
  --env /home/debian/atlas.env`

// fillBotsDashboard loads bots + their chats for the Bots page and overview card.
func (s *Server) fillBotsDashboard(ctx context.Context, u *store.User, data *pageData) {
	s.fillRuntimeBundle(ctx, u, data)
	list, err := s.Store.ListBots(ctx, u.ID)
	if err != nil {
		return
	}
	for _, b := range list {
		bv := botView{
			ID: b.ID, Name: b.Name, Username: b.BotUsername, MachineName: b.MachineName,
			Kind: b.Kind, Version: b.Version, Online: botsmod.Online(b), Legacy: b.Legacy(),
			HasToken: b.HasToken, Pending: b.PendingChats, Approved: b.ApprovedChats,
			Deliveries: b.PendingDeliveries, Instance: b.Instance,
			ProvisionStatus: b.ProvisionStatus, ProvisionError: b.ProvisionError,
		}
		if b.ProvisionAt != nil {
			bv.ProvisionAt = b.ProvisionAt.UTC().Format("2006-01-02 15:04")
		}
		if ch, chat, err := s.Store.ChannelForConsumer(ctx, u.ID,
			store.ConsumerBot, b.ID, store.DirectionReceive); err == nil && ch != nil {
			if atts, _ := s.Store.ConsumerAttachments(ctx, u.ID, store.ConsumerBot, b.ID, store.DirectionReceive); len(atts) > 0 {
				bv.Channel, bv.ChannelChat = ch.Name, chat
			}
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

// fillRuntimeBundle reports presence and inventory of the account's bundle.
// It never unseals it: only the clear metadata columns are read.
func (s *Server) fillRuntimeBundle(ctx context.Context, u *store.User, data *pageData) {
	data.RuntimeBundle.ImportHint = runtimeBundleImportHint
	row, err := s.Store.RuntimeBundle(ctx, u.ID)
	if err != nil || row == nil {
		return
	}
	data.RuntimeBundle.Present = true
	data.RuntimeBundle.Source = row.SourceName
	data.RuntimeBundle.GrokVersion = row.GrokVersion
	data.RuntimeBundle.Updated = row.UpdatedAt.UTC().Format("2006-01-02 15:04")
	data.RuntimeBundle.Components = row.Components
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
	_ = r.ParseForm() // safe-ignore: FormValue parses on demand and reports empty on malformed input
	name := strings.TrimSpace(r.FormValue("name"))
	machineID := strings.TrimSpace(r.FormValue("machine_id"))
	channelID := strings.TrimSpace(r.FormValue("channel_id"))
	bot, token, err := s.Store.CreateBot(r.Context(), u.ID, name, machineID)
	if err != nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	// The channel gives the instance its Telegram credential and primary chat.
	if channelID != "" {
		if err := s.Store.AttachChannel(r.Context(), u.ID, store.ChannelAttachment{
			ChannelID: channelID, Consumer: store.ConsumerBot, ConsumerID: bot.ID,
			Direction: store.DirectionReceive,
		}); err != nil {
			_ = s.Store.DeleteBot(r.Context(), u.ID, bot.ID)
			http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
			return
		}
	}
	_ = s.Store.SetModuleEnabled(r.Context(), u.ID, "bots", true)
	if s.OnToolsChanged != nil {
		s.OnToolsChanged(u.ID)
	}
	// Zero touch: with a channel and a machine there is nothing left to do by hand.
	if channelID != "" && machineID != "" && s.Provision != nil {
		s.Provision.Start(u.ID, bot.ID)
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("Bot created — provisioning started"), http.StatusFound)
		return
	}
	s.flashBotToken(w, token)
	http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("Bot created — copy the token now"), http.StatusFound)
}

// provisionBot installs (or re-installs) the daemon on the bot's machine.
func (s *Server) provisionBot(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id := r.PathValue("id")
	if s.Provision == nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: provisioning is not configured"), http.StatusFound)
		return
	}
	bot, err := s.Store.BotByID(r.Context(), u.ID, id)
	if err != nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: unknown bot"), http.StatusFound)
		return
	}
	if !bot.Provisionable() {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: set a target machine first"), http.StatusFound)
		return
	}
	s.Provision.Start(u.ID, bot.ID)
	http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("Provisioning "+bot.Name+" on "+bot.MachineName), http.StatusFound)
}

// saveBotTarget changes the machine a bot is provisioned onto.
func (s *Server) saveBotTarget(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_ = r.ParseForm() // safe-ignore: FormValue parses on demand and reports empty on malformed input
	if err := s.Store.SetBotTarget(r.Context(), u.ID, r.PathValue("id"), r.FormValue("machine_id")); err != nil {
		http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("error: "+err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/dashboard/bots?flash="+urlQuery("Target machine updated"), http.StatusFound)
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
	_ = s.Store.DetachConsumer(r.Context(), u.ID, store.ConsumerBot, r.PathValue("id"))
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
