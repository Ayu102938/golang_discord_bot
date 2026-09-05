package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"
	"github.com/joho/godotenv"
)

// --- Constants & Types ---

const (
	StepSetChannel = 10
	TimetableFile  = "timetable.csv"
	ChannelsFile   = "channels.csv"
)

type UserSession struct {
	Step        int
	GuildID     string
	LastUpdated time.Time
}

type TimetableEntry struct {
	UserID     string
	GuildID    string
	Title      string
	DateStr    string
	Memo       string
	RemindRule string
}

// --- Global Variables (In-Memory Cache & Mutex) ---

var (
	sessions   = make(map[string]*UserSession)
	sessionsMu sync.Mutex

	// キャッシュデータ
	timetables []TimetableEntry
	channelMap = make(map[string]string)

	// データ保護用Mutex
	dataMu sync.Mutex
)

// --- Main ---

func main() {
	// ログ設定
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if err := godotenv.Load(); err != nil {
		log.Println("could not find .env file (using env vars only)")
	}

	// 起動時にデータをメモリにロード
	loadData()

	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN is not set")
	}

	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildMessages,
				gateway.IntentMessageContent,
				gateway.IntentDirectMessages,
			),
		),
		bot.WithEventListenerFunc(onInteraction),
		bot.WithEventListenerFunc(onMessage),
	)
	if err != nil {
		log.Fatal("Error creating client:", err)
	}

	registerCommands(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err = client.OpenGateway(ctx); err != nil {
		log.Fatal("Error opening gateway:", err)
	}

	go startReminderLoop(ctx, client)

	log.Println("Bot is running. Press CTRL-C to exit.")
	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
	<-s

	log.Println("Shutting down...")
	if err := client.Close(context.Background()); err != nil {
		log.Println("Error closing client:", err)
	}
	cancel() // Stop reminder loop goroutine
}

// --- Data Persistence (Load/Save) ---

func loadData() {
	dataMu.Lock()
	defer dataMu.Unlock()

	// 1. Timetables
	f, err := os.Open(TimetableFile)
	if err == nil {
		defer f.Close()
		records, readErr := csv.NewReader(f).ReadAll()
		if readErr != nil {
			log.Printf("Warning: failed to parse %s, starting with empty data: %v", TimetableFile, readErr)
			records = nil
		}
		timetables = nil // Clear existing
		for _, r := range records {
			if len(r) >= 6 {
				timetables = append(timetables, TimetableEntry{
					UserID:     r[0],
					GuildID:    r[1],
					Title:      r[2],
					DateStr:    r[3],
					Memo:       r[4],
					RemindRule: r[5],
				})
			}
		}
		log.Printf("Loaded %d timetable entries", len(timetables))
	} else {
		log.Println("Timetable file not found, starting empty.")
	}

	// 2. Channels
	fc, err := os.Open(ChannelsFile)
	if err == nil {
		defer fc.Close()
		records, readErr := csv.NewReader(fc).ReadAll()
		if readErr != nil {
			log.Printf("Warning: failed to parse %s, starting with empty data: %v", ChannelsFile, readErr)
			records = nil
		}
		for _, r := range records {
			if len(r) >= 3 {
				key := r[0] + "_" + r[1] // UserID_GuildID
				channelMap[key] = r[2]
			}
		}
		log.Printf("Loaded %d channel configs", len(channelMap))
	} else {
		log.Println("Channel config file not found, starting empty.")
	}
}

// saveTimetables rewrites the entire CSV from memory (safe under lock).
func saveTimetables() {
	// Note: Caller must hold dataMu
	f, err := os.OpenFile(TimetableFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Println("Error saving timetables:", err)
		return
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	for _, t := range timetables {
		w.Write([]string{t.UserID, t.GuildID, t.Title, t.DateStr, t.Memo, t.RemindRule})
	}
}

// addTimetableEntry adds to memory and appends to file.
func addTimetableEntry(t TimetableEntry) error {
	dataMu.Lock()
	defer dataMu.Unlock()

	timetables = append(timetables, t)

	// Append to file for performance (instead of full rewrite)
	f, err := os.OpenFile(TimetableFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	return w.Write([]string{t.UserID, t.GuildID, t.Title, t.DateStr, t.Memo, t.RemindRule})
}

// deleteTimetableEntry removes from memory and rewrites file.
func deleteTimetableEntry(userID, guildID string, targetIndex int) error {
	dataMu.Lock()
	defer dataMu.Unlock()

	newTables := []TimetableEntry{}
	userCount := 0
	found := false

	for _, t := range timetables {
		if t.UserID == userID && t.GuildID == guildID {
			userCount++
			if userCount == targetIndex {
				found = true
				continue // Skip this one (delete)
			}
		}
		newTables = append(newTables, t)
	}

	if !found {
		return fmt.Errorf("not found")
	}

	timetables = newTables
	saveTimetables() // Refresh file
	return nil
}

// saveChannels rewrites the entire channels.csv from memory (safe under lock).
// Note: Caller must hold dataMu.
func saveChannels() {
	f, err := os.OpenFile(ChannelsFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Println("Error saving channels:", err)
		return
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	// channelMap keys are "UserID_GuildID" — split them back into columns for CSV.
	for key, channelID := range channelMap {
		parts := strings.SplitN(key, "_", 2)
		if len(parts) != 2 {
			continue
		}
		w.Write([]string{parts[0], parts[1], channelID})
	}
}

// updateChannelConfig updates memory and rewrites the channels file (no duplicates).
func updateChannelConfig(userID, guildID, channelID string) error {
	dataMu.Lock()
	defer dataMu.Unlock()

	key := userID + "_" + guildID
	channelMap[key] = channelID

	saveChannels() // Rewrite file to avoid duplicate rows
	return nil
}

// --- Interaction Handlers ---

func onInteraction(e *events.InteractionCreate) {
	var userID, guildID string

	// InteractionCreate event itself doesn't guarantee direct User access in all contexts easily via methods
	// in strict type systems without casting, but disgo generally exposes it.
	// However, e.User() should work if it's available.
	// Let's rely on the specific interaction types which usually have a User/Member field.

	switch i := e.Interaction.(type) {
	case discord.ApplicationCommandInteraction:
		if i.User().ID.String() != "" {
			userID = i.User().ID.String()
		} else if i.Member().User.ID.String() != "" {
			userID = i.Member().User.ID.String()
		}
		if i.GuildID() != nil {
			guildID = i.GuildID().String()
		}
		handleSlashCommand(e, i, userID, guildID)

	case discord.ModalSubmitInteraction:
		if i.User().ID.String() != "" {
			userID = i.User().ID.String()
		} else if i.Member().User.ID.String() != "" {
			userID = i.Member().User.ID.String()
		}
		if i.GuildID() != nil {
			guildID = i.GuildID().String()
		}
		handleModalSubmit(e, i, userID, guildID)
	}
}

func handleSlashCommand(e *events.InteractionCreate, d discord.ApplicationCommandInteraction, userID, guildID string) {
	name := d.Data.CommandName()

	// Helper to send simple replies
	reply := func(msg string, ephemeral bool) {
		e.Respond(discord.InteractionResponseTypeCreateMessage,
			discord.NewMessageCreateBuilder().SetContent(msg).SetEphemeral(ephemeral).Build())
	}

	if name == "timetable" || name == "setch" || name == "list" || name == "delete" {
		if guildID == "" {
			reply("このコマンドはサーバー内でのみ使用可能です", true)
			return
		}
	}

	switch name {
	case "timetable":
		// Check channel config
		dataMu.Lock()
		_, ok := channelMap[userID+"_"+guildID]
		dataMu.Unlock()

		if !ok {
			reply("/setch で先に通知先を設定してください", true)
			return
		}

		// Show Modal
		e.Respond(discord.InteractionResponseTypeModal, discord.ModalCreate{
			CustomID: "timetable_modal",
			Title:    "予定の登録",
			Components: []discord.ContainerComponent{
				discord.ActionRowComponent{
					discord.TextInputComponent{
						CustomID:    "title",
						Style:       discord.TextInputStyleShort,
						Label:       "タイトル",
						Placeholder: "例: 会議, 課題",
						Required:    true,
					},
				},
				discord.ActionRowComponent{
					discord.TextInputComponent{
						CustomID:    "date",
						Style:       discord.TextInputStyleShort,
						Label:       "日付 (yyyy/mm/dd)",
						Placeholder: "例: 2026/01/24",
						Required:    true,
					},
				},
				discord.ActionRowComponent{
					discord.TextInputComponent{
						CustomID:    "memo",
						Style:       discord.TextInputStyleParagraph,
						Label:       "メモ",
						Placeholder: "補足情報など（任意）",
						Required:    false,
					},
				},
				discord.ActionRowComponent{
					discord.TextInputComponent{
						CustomID:    "remind",
						Style:       discord.TextInputStyleShort,
						Label:       "リマインド時期 (0=当日0時, 1d, 12h)",
						Placeholder: "例: 1d, 3h, 0",
						Required:    true,
					},
				},
			},
		})

	case "setch":
		sessionsMu.Lock()
		sessions[userID] = &UserSession{Step: StepSetChannel, GuildID: guildID, LastUpdated: time.Now()}
		sessionsMu.Unlock()
		reply("通知先のチャンネルをメンションして入力してください (例: #雑談)", false)

	case "list":
		dataMu.Lock()
		var myEvents []TimetableEntry
		for _, t := range timetables {
			if t.UserID == userID && t.GuildID == guildID {
				myEvents = append(myEvents, t)
			}
		}
		dataMu.Unlock()

		if len(myEvents) == 0 {
			reply("登録されている予定はありません。", false)
			return
		}

		var sb strings.Builder
		sb.WriteString("📋 **あなたの予定一覧**\n")
		for i, t := range myEvents {
			memo := ""
			if t.Memo != "" {
				memo = fmt.Sprintf(" [%s]", t.Memo)
			}
			sb.WriteString(fmt.Sprintf("%d. %s (%s)%s - 通知: %s\n", i+1, t.Title, t.DateStr, memo, t.RemindRule))
		}
		reply(sb.String(), false)

	case "delete":
		var index int
		if sd, ok := d.Data.(discord.SlashCommandInteractionData); ok {
			index = sd.Int("index")
		}
		if err := deleteTimetableEntry(userID, guildID, index); err != nil {
			reply("削除に失敗しました。番号を確認してください。", true)
		} else {
			reply(fmt.Sprintf("%d 番の予定を削除しました。", index), false)
		}

	case "testremind":
		checkAndSendReminders(e.Client(), true) // Force check
		reply("リマインドの強制実行を完了しました。", true)
	}
}

func handleModalSubmit(e *events.InteractionCreate, d discord.ModalSubmitInteraction, userID, guildID string) {
	data := d.Data
	title := data.Text("title")
	dateStr := data.Text("date")
	memo := data.Text("memo")
	remindRule := data.Text("remind")

	reply := func(msg string, ephemeral bool) {
		e.Respond(discord.InteractionResponseTypeCreateMessage,
			discord.NewMessageCreateBuilder().SetContent(msg).SetEphemeral(ephemeral).Build())
	}

	// Validate
	if _, err := time.Parse("2006/01/02", dateStr); err != nil {
		reply("❌ 日付の形式が正しくありません (yyyy/mm/dd)", true)
		return
	}
	duration, err := parseCustomDuration(remindRule)
	if err != nil {
		reply("❌ リマインド時期の形式が正しくありません (例: 1d, 3h, 0)", true)
		return
	}

	// Add
	entry := TimetableEntry{
		UserID:     userID,
		GuildID:    guildID,
		Title:      title,
		DateStr:    dateStr,
		Memo:       memo,
		RemindRule: remindRule,
	}
	if err := addTimetableEntry(entry); err != nil {
		log.Println("Error adding entry:", err)
		reply("❌ 保存中にエラーが発生しました。", true)
	} else {
		reply(fmt.Sprintf("✅ 予定を保存しました！\n予定: %s\n日付: %s\n通知: %s 前", title, dateStr, duration.String()), false)
	}
}

func onMessage(e *events.MessageCreate) {
	if e.Message.Author.Bot {
		return
	}
	userID := e.Message.Author.ID.String()

	sessionsMu.Lock()
	session, exists := sessions[userID]
	if exists && session.Step == StepSetChannel {
		session.LastUpdated = time.Now() // Refresh timer on user activity
	}
	sessionsMu.Unlock()

	if exists && session.Step == StepSetChannel {
		input := strings.TrimSpace(e.Message.Content)
		if strings.HasPrefix(input, "<#") && strings.HasSuffix(input, ">") {
			channelID := input[2 : len(input)-1]
			if err := updateChannelConfig(userID, session.GuildID, channelID); err != nil {
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("設定の保存に失敗しました").Build())
			} else {
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent(fmt.Sprintf("通知先を <#%s> に設定しました", channelID)).Build())
			}
		} else {
			e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("チャンネルをメンションしてください").Build())
			return
		}
		sessionsMu.Lock()
		if sessions[userID] == session { // only delete if it's the same session we read
			delete(sessions, userID)
		}
		sessionsMu.Unlock()
	}
}

// --- Reminders ---

func startReminderLoop(ctx context.Context, client bot.Client) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("Reminder loop stopped")
			return
		case <-ticker.C:
			checkAndSendReminders(client, false)
			cleanupExpiredSessions()
		}
	}
}

// cleanupExpiredSessions removes sessions that have been inactive for more than 30 minutes.
func cleanupExpiredSessions() {
	const sessionTTL = 30 * time.Minute
	cutoff := time.Now().Add(-sessionTTL)

	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	removed := 0
	for userID, s := range sessions {
		if s.LastUpdated.Before(cutoff) {
			delete(sessions, userID)
			removed++
		}
	}
	if removed > 0 {
		log.Printf("Cleaned up %d expired session(s)", removed)
	}
}

func checkAndSendReminders(client bot.Client, force bool) {
	dataMu.Lock()
	// Snapshot for thread-safe reading without holding lock during network calls
	currentTables := make([]TimetableEntry, len(timetables))
	copy(currentTables, timetables)

	currentChannels := make(map[string]string)
	for k, v := range channelMap {
		currentChannels[k] = v
	}
	dataMu.Unlock()

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	type removalTarget struct {
		UserID  string
		GuildID string
		Title   string
		DateStr string // Use these as composite key
	}
	var targets []removalTarget

	for _, t := range currentTables {
		targetDate, err := time.Parse("2006/01/02", t.DateStr)
		if err != nil {
			continue // Invalid date, maybe should remove?
		}

		// Calculate notify time
		eventTime := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 0, 0, 0, 0, time.Local)
		duration, _ := parseCustomDuration(t.RemindRule)
		notifyTime := eventTime.Add(-duration)

		shouldNotify := false
		shouldRemove := false

		// 1. Is it past?
		if targetDate.Before(today) {
			shouldRemove = true
		}

		// 2. Is it time?
		diff := now.Sub(notifyTime)
		if force || (diff >= -35*time.Second && diff < 35*time.Second) {
			shouldNotify = true
		}

		if shouldNotify {
			key := t.UserID + "_" + t.GuildID
			chID, hasCh := currentChannels[key]
			msg := fmt.Sprintf("🔔 <@%s> **リマインド: 「%s」の予定**\n日付: %s", t.UserID, t.Title, t.DateStr)
			if t.Memo != "" {
				msg += "\nメモ: " + t.Memo
			}

			if hasCh {
				sfID, err := snowflakeID(chID)
				if err != nil {
					log.Printf("Skipping reminder for %q: invalid channel ID %q", t.Title, chID)
					continue
				}
				client.Rest().CreateMessage(sfID, discord.NewMessageCreateBuilder().SetContent(msg).Build())
			} else {
				// DM fallback
				userSF, err := snowflakeID(t.UserID)
				if err != nil {
					log.Printf("Skipping DM for %q: invalid user ID %q", t.Title, t.UserID)
					continue
				}
				ch, err := client.Rest().CreateDMChannel(userSF)
				if err == nil {
					client.Rest().CreateMessage(ch.ID(), discord.NewMessageCreateBuilder().SetContent(msg).Build())
				}
			}
		}

		if shouldRemove {
			targets = append(targets, removalTarget{t.UserID, t.GuildID, t.Title, t.DateStr})
		}
	}

	if len(targets) > 0 && !force {
		dataMu.Lock()
		defer dataMu.Unlock()

		// Rebuild timetables excluding targets
		var kept []TimetableEntry
	Outer:
		for _, t := range timetables {
			for _, target := range targets {
				if t.UserID == target.UserID && t.GuildID == target.GuildID && t.Title == target.Title && t.DateStr == target.DateStr {
					continue Outer // Drop this one
				}
			}
			kept = append(kept, t)
		}
		timetables = kept
		saveTimetables() // Save changes
	}
}

// --- Utils ---

func registerCommands(client bot.Client) {
	commands := []discord.ApplicationCommandCreate{
		discord.SlashCommandCreate{
			Name:        "timetable",
			Description: "タイムテーブルの入力フォームを表示します",
		},
		discord.SlashCommandCreate{
			Name:        "setch",
			Description: "通知先のチャンネルを設定します",
		},
		discord.SlashCommandCreate{
			Name:        "list",
			Description: "登録されているタイムテーブルを表示します",
		},
		discord.SlashCommandCreate{
			Name:        "delete",
			Description: "タイムテーブルを削除します",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionInt{
					Name:        "index",
					Description: "削除する予定の番号（listで確認してください）",
					Required:    true,
				},
			},
		},
		discord.SlashCommandCreate{
			Name:        "testremind",
			Description: "現在時刻でリマインドを強制実行します（テスト用）",
		},
	}

	if _, err := client.Rest().SetGlobalCommands(client.ApplicationID(), commands); err != nil {
		log.Println("Failed to register commands:", err)
	} else {
		log.Println("Successfully registered global slash commands")
	}
}

func parseCustomDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		var d float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%f", &d); err != nil {
			return 0, err
		}
		return time.Duration(d * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func snowflakeID(id string) (snowflake.ID, error) {
	s, err := snowflake.Parse(id)
	if err != nil {
		log.Printf("Warning: failed to parse snowflake ID %q: %v", id, err)
		return 0, err
	}
	return s, nil
}
