package store

import (
	"context"
	"testing"
	"time"
)

func newBotsStore(t *testing.T) (*Store, *User, context.Context) {
	t.Helper()
	st, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	u, err := st.CreateUserOpts(ctx, "bots@example.com", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	return st, u, ctx
}

func TestCreateBotAndTokenLookup(t *testing.T) {
	st, u, ctx := newBotsStore(t)

	b, token, err := st.CreateBot(ctx, u.ID, "Atlas", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "Atlas" || b.Kind != BotKindDaemon || !b.HasToken || token == "" {
		t.Fatalf("unexpected bot: %+v token=%q", b, token)
	}

	got, err := st.BotByToken(ctx, token)
	if err != nil || got.ID != b.ID {
		t.Fatalf("BotByToken: %v %+v", err, got)
	}
	if _, err := st.BotByToken(ctx, "nope"); err == nil {
		t.Fatal("unknown token must not resolve")
	}
	if _, err := st.BotByUserAndName(ctx, u.ID, "atlas"); err != nil {
		t.Fatalf("name lookup is case-insensitive: %v", err)
	}

	// Names are unique per user.
	if _, _, err := st.CreateBot(ctx, u.ID, "Atlas", ""); err == nil {
		t.Fatal("duplicate bot name should fail")
	}

	// Another user cannot see it.
	other, err := st.CreateUserOpts(ctx, "other@example.com", "password2", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListBots(ctx, other.ID); len(list) != 0 {
		t.Fatalf("cross-tenant leak: %d bots", len(list))
	}
	if _, err := st.BotByID(ctx, other.ID, b.ID); err == nil {
		t.Fatal("cross-tenant read should fail")
	}
}

func TestReportBotChatIsIdempotent(t *testing.T) {
	st, u, ctx := newBotsStore(t)
	b, _, err := st.CreateBot(ctx, u.ID, "Atlas", "")
	if err != nil {
		t.Fatal(err)
	}

	c, created, err := st.ReportBotChat(ctx, b.ID, u.ID, BotChat{
		ChatID: "282611642", Type: "private", Title: "Jairo", Username: "@kidandcat",
		FirstMessage: "hola",
	})
	if err != nil || !created {
		t.Fatalf("first report: err=%v created=%v", err, created)
	}
	if c.Status != BotChatPending || c.Type != BotChatPrivate || c.Username != "kidandcat" {
		t.Fatalf("unexpected chat: %+v", c)
	}

	// Repeat: no new row, no second notification, snippet preserved.
	c2, created2, err := st.ReportBotChat(ctx, b.ID, u.ID, BotChat{
		ChatID: "282611642", Type: "private", FirstMessage: "otra cosa",
	})
	if err != nil || created2 {
		t.Fatalf("repeat report: err=%v created=%v", err, created2)
	}
	if c2.FirstMessage != "hola" || c2.Title != "Jairo" {
		t.Fatalf("repeat must not overwrite first message / title: %+v", c2)
	}
	if n, _ := st.CountBotChats(ctx, b.ID, ""); n != 1 {
		t.Fatalf("expected 1 chat, got %d", n)
	}

	// Supergroup normalises to group and keeps the title.
	g, created, err := st.ReportBotChat(ctx, b.ID, u.ID, BotChat{
		ChatID: "-1001234", Type: "supergroup", Title: "Casa",
	})
	if err != nil || !created || g.Type != BotChatGroup || g.Title != "Casa" {
		t.Fatalf("group report: err=%v created=%v chat=%+v", err, created, g)
	}
}

func TestDecideBotChatAndCursor(t *testing.T) {
	st, u, ctx := newBotsStore(t)
	b, _, err := st.CreateBot(ctx, u.ID, "Atlas", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReportBotChat(ctx, b.ID, u.ID, BotChat{ChatID: "1", Type: "private", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReportBotChat(ctx, b.ID, u.ID, BotChat{ChatID: "2", Type: "group", Title: "B"}); err != nil {
		t.Fatal(err)
	}

	pending, names, err := st.ListPendingBotChats(ctx, u.ID)
	if err != nil || len(pending) != 2 || names[b.ID] != "Atlas" {
		t.Fatalf("pending: err=%v n=%d names=%v", err, len(pending), names)
	}

	before, err := st.BotChatsUpdatedAt(ctx, b.ID)
	if err != nil || before == "" {
		t.Fatalf("cursor: %v %q", err, before)
	}
	time.Sleep(1100 * time.Millisecond) // RFC3339 second resolution

	c, err := st.DecideBotChat(ctx, u.ID, b.ID, "1", BotChatApproved, "panel")
	if err != nil || c.Status != BotChatApproved || c.DecidedBy != "panel" || c.DecidedAt == nil {
		t.Fatalf("approve: %v %+v", err, c)
	}

	// updated_since only returns what changed after the cursor.
	changed, err := st.ListBotChats(ctx, b.ID, "", before)
	if err != nil || len(changed) != 1 || changed[0].ChatID != "1" {
		t.Fatalf("updated_since: err=%v got=%+v", err, changed)
	}

	// A later report must not reset an approved chat to pending.
	again, created, err := st.ReportBotChat(ctx, b.ID, u.ID, BotChat{ChatID: "1", Type: "private"})
	if err != nil || created || again.Status != BotChatApproved {
		t.Fatalf("approved chat must stay approved: err=%v created=%v chat=%+v", err, created, again)
	}

	if _, err := st.DecideBotChat(ctx, u.ID, b.ID, "404", BotChatDenied, "panel"); err == nil {
		t.Fatal("deciding an unknown chat should fail")
	}

	// Counters land on the bot rows.
	list, err := st.ListBots(ctx, u.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list bots: %v", err)
	}
	if list[0].ApprovedChats != 1 || list[0].PendingChats != 1 {
		t.Fatalf("counters: %+v", list[0])
	}

	// Deleting the bot cascades its chats away.
	if err := st.DeleteBot(ctx, u.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountBotChatsPending(ctx, u.ID); n != 0 {
		t.Fatalf("chats should cascade, %d left", n)
	}
}

func TestUpdateBotIdentityLinksMachine(t *testing.T) {
	st, u, ctx := newBotsStore(t)
	mac, _, err := st.CreateMachine(ctx, u.ID, "vps2")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := st.CreateBot(ctx, u.ID, "Atlas", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.UpdateBotIdentity(ctx, b.ID, "@atlas_bot", "vps2", "1.2.0"); err != nil {
		t.Fatal(err)
	}
	got, err := st.BotByID(ctx, u.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BotUsername != "atlas_bot" || got.MachineID != mac.ID || got.MachineName != "vps2" || got.Version != "1.2.0" {
		t.Fatalf("identity: %+v", got)
	}
	if got.LastSeen == nil {
		t.Fatal("register should count as a heartbeat")
	}

	// Empty fields keep previous values; an unknown machine name is kept as a label.
	if err := st.UpdateBotIdentity(ctx, b.ID, "", "unknown-host", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.BotByID(ctx, u.ID, b.ID)
	if got.BotUsername != "atlas_bot" || got.Version != "1.2.0" {
		t.Fatalf("empty fields must not clear: %+v", got)
	}
	if got.MachineID != mac.ID {
		t.Fatalf("unknown machine name must not unlink: %+v", got)
	}
}

// Legacy owners are seeded for accounts that already exist when the module
// migration runs, so machine_ai_run keeps resolving them. A brand new install
// (no users yet) starts with an empty registry.
func TestLegacyOwnersSeededOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUserOpts(ctx, "upgrade@example.com", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListBots(ctx, u.ID); len(list) != 0 {
		t.Fatalf("a new account should not be seeded: %v", list)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: the migration seeds the accounts that already existed.
	st, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	list, err := st.ListBots(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]Bot{}
	for _, b := range list {
		seen[b.Name] = b
	}
	for _, name := range LegacyBotOwners {
		b, ok := seen[name]
		if !ok {
			t.Fatalf("legacy owner %q was not seeded: %v", name, seen)
		}
		if !b.Legacy() || b.HasToken {
			t.Fatalf("legacy row %q should have no token: %+v", name, b)
		}
	}
	// machine_ai_run owner=Minerva keeps resolving after the migration.
	if _, err := st.BotByUserAndName(ctx, u.ID, "minerva"); err != nil {
		t.Fatalf("legacy owner lookup: %v", err)
	}
	// Seeding twice must not duplicate.
	if err := st.SeedLegacyBots(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := st.ListBots(ctx, u.ID)
	if len(after) != len(list) {
		t.Fatalf("re-seed duplicated rows: %d -> %d", len(list), len(after))
	}
}

func TestIssueBotTokenPromotesLegacyRow(t *testing.T) {
	st, u, ctx := newBotsStore(t)
	if err := st.SeedLegacyBots(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.BotByUserAndName(ctx, u.ID, "Minerva")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.IssueBotToken(ctx, u.ID, legacy.ID)
	if err != nil || token == "" {
		t.Fatalf("IssueBotToken: %v %q", err, token)
	}
	got, err := st.BotByToken(ctx, token)
	if err != nil || got.ID != legacy.ID {
		t.Fatalf("token lookup: %v %+v", err, got)
	}
	if got.Legacy() || !got.HasToken {
		t.Fatalf("row should be a daemon now: %+v", got)
	}
	// Reissuing invalidates the old token.
	next, err := st.IssueBotToken(ctx, u.ID, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.BotByToken(ctx, token); err == nil {
		t.Fatal("old token still works after reissue")
	}
	if _, err := st.BotByToken(ctx, next); err != nil {
		t.Fatalf("new token: %v", err)
	}
	// Cross-tenant reissue must fail.
	other, err := st.CreateUserOpts(ctx, "issue-other@example.com", "password2", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.IssueBotToken(ctx, other.ID, legacy.ID); err == nil {
		t.Fatal("cross-tenant reissue should fail")
	}
}

func TestBotJobAttributionAndDeliveryRetry(t *testing.T) {
	st, u, ctx := newBotsStore(t)
	b, _, err := st.CreateBot(ctx, u.ID, "Atlas-jobs", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordBotJob(ctx, "job-1", b.ID, u.ID, "123", "vps2"); err != nil {
		t.Fatal(err)
	}
	// Idempotent, and an empty chat_id never clears the recorded one.
	if err := st.RecordBotJob(ctx, "job-1", b.ID, u.ID, "", "vps2"); err != nil {
		t.Fatal(err)
	}
	link, err := st.BotJobByID(ctx, "job-1")
	if err != nil || link.ChatID != "123" || link.BotID != b.ID {
		t.Fatalf("BotJobByID: %v %+v", err, link)
	}

	if _, err := st.EnqueueBotDelivery(ctx, b.ID, u.ID, BotDeliveryAIJobResult, "", []byte(`{"job_id":"job-1"}`)); err != nil {
		t.Fatal(err)
	}
	first, err := st.ListBotDeliveries(ctx, b.ID, 0)
	if err != nil || len(first) != 1 || first[0].Attempts != 1 {
		t.Fatalf("first fetch: %v %+v", err, first)
	}
	// Invisible while in flight.
	if again, _ := st.ListBotDeliveries(ctx, b.ID, 0); len(again) != 0 {
		t.Fatalf("in-flight delivery handed out again: %+v", again)
	}
	// Once the visibility timeout lapses it comes back (daemon crashed before acking).
	if _, err := st.db.ExecContext(ctx,
		`UPDATE bot_deliveries SET fetched_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-2*DeliveryRetryAfter).Format(time.RFC3339), first[0].ID); err != nil {
		t.Fatal(err)
	}
	retry, _ := st.ListBotDeliveries(ctx, b.ID, 0)
	if len(retry) != 1 || retry[0].Attempts != 2 {
		t.Fatalf("retry: %+v", retry)
	}
	if n, err := st.AckBotDeliveries(ctx, b.ID, []string{first[0].ID}); err != nil || n != 1 {
		t.Fatalf("ack: %v %d", err, n)
	}
	if n, _ := st.CountBotDeliveries(ctx, b.ID); n != 0 {
		t.Fatalf("pending after ack: %d", n)
	}
	// Deleting the bot drops its outbox and job attributions.
	if err := st.DeleteBot(ctx, u.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BotJobByID(ctx, "job-1"); err == nil {
		t.Fatal("bot_jobs row should cascade with the bot")
	}
}

// A bot created before provisioning existed must get a unit name on upgrade,
// otherwise it can never be provisioned without being recreated.
func TestInstanceBackfilledOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	st, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUserOpts(ctx, "backfill@example.com", "password1", CreateUserOpts{AllowOpen: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := st.CreateBot(ctx, u.ID, "Atlas", "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-provisioning row shape.
	if _, err := st.db.ExecContext(ctx, `UPDATE bots SET instance = '' WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	got, err := st.BotByID(ctx, u.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Instance != "atlas" {
		t.Fatalf("instance should be backfilled, got %q", got.Instance)
	}
}

// A daemon reports its raw hostname, which must not clobber the machine name the
// operator configured in the panel.
func TestRegistrationKeepsConfiguredMachineName(t *testing.T) {
	st, u, ctx := newBotsStore(t)
	mac, _, err := st.CreateMachine(ctx, u.ID, "vps2")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := st.CreateBot(ctx, u.ID, "Atlas", mac.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon announces the host's hostname, not the configured name.
	if err := st.UpdateBotIdentity(ctx, b.ID, "mavis_es_bot", "vps-068ca265", "0.3.1"); err != nil {
		t.Fatal(err)
	}
	got, err := st.BotByID(ctx, u.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineName != "vps2" {
		t.Fatalf("hostname overwrote the configured machine name: %q", got.MachineName)
	}
	if got.MachineID != mac.ID {
		t.Fatalf("machine link lost: %q", got.MachineID)
	}
	// A name that does resolve to one of this account's machines is adopted.
	if _, _, err := st.CreateMachine(ctx, u.ID, "vps3"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateBotIdentity(ctx, b.ID, "", "vps3", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.BotByID(ctx, u.ID, b.ID)
	if got.MachineName != "vps3" {
		t.Fatalf("a real machine name should be adopted: %q", got.MachineName)
	}

	// A bot with no machine yet still learns something useful.
	fresh, _, err := st.CreateBot(ctx, u.ID, "Loose", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateBotIdentity(ctx, fresh.ID, "", "some-host", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.BotByID(ctx, u.ID, fresh.ID)
	if got.MachineName != "some-host" {
		t.Fatalf("an unconfigured bot should record what it reports: %q", got.MachineName)
	}
}
