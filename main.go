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
	"github.com/disgoorg/snowflake/v2" // IDを扱うためのパッケージ
	"github.com/joho/godotenv"
)

const (
	StepSetChannel = 10
)

type UserSession struct {
	Step    int
	GuildID string // サーバーID保存用
}

var sessions = make(map[string]*UserSession)
var mu sync.Mutex

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("could not find .env file")
	}

	token := os.Getenv("DISCORD_BOT_TOKEN")

	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildMessages,
				gateway.IntentMessageContent,
				gateway.IntentDirectMessages,
			),
		),
		// インタラクション（スラッシュコマンド、モーダル送信）の処理
		bot.WithEventListenerFunc(func(e *events.InteractionCreate) {
			userID := e.User().ID.String()
			guildID := ""
			if e.GuildID() != nil {
				guildID = e.GuildID().String()
			}

			switch i := e.Interaction.(type) {
			case discord.ApplicationCommandInteraction:
				data := i.Data
				switch data.CommandName() {
				case "timetable":
					if guildID == "" {
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("このコマンドはサーバー内でのみ使用可能です").SetEphemeral(true).Build())
						return
					}
					// チャンネル設定チェック
					configs := loadChannelConfigs()
					if _, ok := configs[userID+"_"+guildID]; !ok {
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("/setch で先に通知先を設定してください").SetEphemeral(true).Build())
						return
					}

					// モーダルの作成（Builderを使わずに直書き）
					modal := discord.ModalCreate{
						CustomID: "timetable_modal",
						Title:    "予定の登録",
						Components: []discord.ContainerComponent{
							discord.ActionRowComponent{
								discord.TextInputComponent{
									CustomID:    "title",
									Style:       discord.TextInputStyleShort,
									Label:       "タイトル",
									Placeholder: "例: 会議, 燃えるゴミ",
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
									Label:       "リマインド時期 (0=当日, 1d, 12h)",
									Placeholder: "例: 1d, 3h, 0",
									Required:    true,
								},
							},
						},
					}

					_ = e.Respond(discord.InteractionResponseTypeModal, modal)

				case "setch":
					if guildID == "" {
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("このコマンドはサーバー内でのみ使用可能です").SetEphemeral(true).Build())
						return
					}
					mu.Lock()
					sessions[userID] = &UserSession{
						Step:    StepSetChannel,
						GuildID: guildID,
					}
					mu.Unlock()
					_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("通知先のチャンネルをメンションして入力してください (例: #雑談)").Build())

				case "list":
					if guildID == "" {
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("このコマンドはサーバー内でのみ使用可能です").SetEphemeral(true).Build())
						return
					}
					timetables := getUserTimetables(userID, guildID)
					if len(timetables) == 0 {
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("登録されている予定はありません。").Build())
					} else {
						var sb strings.Builder
						sb.WriteString("📋 **あなたの予定一覧**\n")
						for i, t := range timetables {
							memoDisplay := ""
							if t.Memo != "" {
								memoDisplay = fmt.Sprintf(" [%s]", t.Memo)
							}
							sb.WriteString(fmt.Sprintf("%d. %s (%s)%s - 通知: %s\n", i+1, t.Title, t.Date, memoDisplay, t.RemindRule))
						}
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent(sb.String()).Build())
					}

				case "delete":
					if guildID == "" {
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("このコマンドはサーバー内でのみ使用可能です").SetEphemeral(true).Build())
						return
					}

					index := 0
					if sd, ok := i.Data.(discord.SlashCommandInteractionData); ok {
						index = sd.Int("index")
					}

					err := deleteUserTimetableEntry(userID, guildID, index)
					if err != nil {
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("削除に失敗しました。番号を確認してください。").SetEphemeral(true).Build())
					} else {
						_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent(fmt.Sprintf("%d 番の予定を削除しました。", index)).Build())
					}

				case "testremind":
					checkAndSendReminders(e.Client(), true, userID)
					_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("リマインドの強制実行を完了しました。").SetEphemeral(true).Build())
				}

			case discord.ModalSubmitInteraction:
				modalData := i.Data
				title := modalData.Text("title")
				dateStr := modalData.Text("date")
				memo := modalData.Text("memo")
				remindRule := modalData.Text("remind")

				// バリデーション
				if _, err := time.Parse("2006/01/02", dateStr); err != nil {
					_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("❌ 日付の形式が正しくありません (yyyy/mm/dd)").SetEphemeral(true).Build())
					return
				}
				duration, err := parseCustomDuration(remindRule)
				if err != nil {
					_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("❌ リマインド時期の形式が正しくありません (例: 1d, 3h, 0)").SetEphemeral(true).Build())
					return
				}

				// 保存
				saveErr := saveToCSV(userID, guildID, title, dateStr, memo, remindRule)
				if saveErr != nil {
					log.Println("CSV error:", saveErr)
					_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent("❌ 保存中にエラーが発生しました。").SetEphemeral(true).Build())
				} else {
					msg := fmt.Sprintf("✅ 予定を保存しました！\n予定: %s\n日付: %s\n通知: %s 前", title, dateStr, duration.String())
					_ = e.Respond(discord.InteractionResponseTypeCreateMessage, discord.NewMessageCreateBuilder().SetContent(msg).Build())
				}
			}
		}),
		// 従来のメッセージ入力（通知設定など）の処理
		bot.WithEventListenerFunc(func(e *events.MessageCreate) {
			if e.Message.Author.Bot {
				return
			}
			userID := e.Message.Author.ID.String()

			mu.Lock()
			session, exists := sessions[userID]
			mu.Unlock()

			if exists {
				handleConversationalFlow(e, session)
			}
		}),
	)
	if err != nil {
		panic(err)
	}

	// コマンドの登録
	registerCommands(client)

	if err = client.OpenGateway(context.TODO()); err != nil {
		panic(err)
	}

	go startReminderLoop(client)

	log.Println("Bot is now running. Press CTRL-C to exit.")
	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
	<-s
}

func registerCommands(client bot.Client) {
	commands := []discord.ApplicationCommandCreate{
		discord.SlashCommandCreate{
			Name:        "timetable",
			Description: "予定の登録フォームを表示します",
		},
		discord.SlashCommandCreate{
			Name:        "setch",
			Description: "通知先のチャンネルを設定します",
		},
		discord.SlashCommandCreate{
			Name:        "list",
			Description: "登録されている予定を表示します",
		},
		discord.SlashCommandCreate{
			Name:        "delete",
			Description: "予定を削除します",
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

func handleConversationalFlow(e *events.MessageCreate, session *UserSession) {
	userID := e.Message.Author.ID.String()

	switch session.Step {
	case StepSetChannel:
		input := strings.TrimSpace(e.Message.Content)
		if strings.HasPrefix(input, "<#") && strings.HasSuffix(input, ">") {
			channelID := input[2 : len(input)-1]
			if err := saveChannelConfig(userID, session.GuildID, channelID); err != nil {
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("設定の保存に失敗しました").Build())
			} else {
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent(fmt.Sprintf("通知先を <#%s> に設定しました", channelID)).Build())
			}
		} else {
			e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("チャンネルをメンションしてください").Build())
			return
		}
		mu.Lock()
		delete(sessions, userID)
		mu.Unlock()
	}
}

// --- 以下、既存のロジック ---

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

func saveToCSV(userID, guildID, title, date, memo, remindRule string) error {
	f, err := os.OpenFile("timetable.csv", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := csv.NewWriter(f)
	defer writer.Flush()
	return writer.Write([]string{userID, guildID, title, date, memo, remindRule})
}

func saveChannelConfig(userID, guildID, channelID string) error {
	f, err := os.OpenFile("channels.csv", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := csv.NewWriter(f)
	defer writer.Flush()
	return writer.Write([]string{userID, guildID, channelID})
}

func startReminderLoop(client bot.Client) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		checkAndSendReminders(client, false, "")
	}
}

func checkAndSendReminders(client bot.Client, force bool, targetUserID string) {
	f, err := os.Open("timetable.csv")
	if err != nil {
		return
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return
	}

	channelMap := loadChannelConfigs()
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	var validRecords [][]string
	hasChanges := false

	for _, record := range records {
		if len(record) < 5 {
			continue
		}
		userID, guildID, title, dateStr := record[0], record[1], record[2], record[3]
		if targetUserID != "" && userID != targetUserID {
			validRecords = append(validRecords, record)
			continue
		}

		memo := ""
		remindRule := "1d"
		if len(record) >= 6 {
			memo, remindRule = record[4], record[5]
		} else {
			remindRule = record[4]
		}

		targetDate, err := time.Parse("2006/01/02", dateStr)
		if err != nil {
			continue
		}

		if targetDate.Before(today) && !force {
			hasChanges = true
			continue
		}
		validRecords = append(validRecords, record)

		eventTime := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 9, 0, 0, 0, time.Local)
		duration, _ := parseCustomDuration(remindRule)
		notifyTime := eventTime.Add(-duration)

		diff := now.Sub(notifyTime)
		if force || (diff >= -35*time.Second && diff < 35*time.Second) {
			if targetChannelID, ok := channelMap[userID+"_"+guildID]; ok {
				sendNotification(client, targetChannelID, userID, title, dateStr, memo)
			} else {
				sendDM(client, userID, title, dateStr, memo)
			}
		}
	}

	if hasChanges && !force {
		wFile, _ := os.OpenFile("timetable.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		defer wFile.Close()
		writer := csv.NewWriter(wFile)
		writer.WriteAll(validRecords)
		writer.Flush()
	}
}

func loadChannelConfigs() map[string]string {
	m := make(map[string]string)
	f, err := os.Open("channels.csv")
	if err != nil {
		return m
	}
	defer f.Close()
	records, _ := csv.NewReader(f).ReadAll()
	for _, row := range records {
		if len(row) >= 3 {
			m[row[0]+"_"+row[1]] = row[2]
		}
	}
	return m
}

func getUserTimetables(targetUserID, targetGuildID string) []TimetableEntry {
	var entries []TimetableEntry
	f, _ := os.Open("timetable.csv")
	defer f.Close()
	records, _ := csv.NewReader(f).ReadAll()
	for _, r := range records {
		if len(r) >= 5 && r[0] == targetUserID && r[1] == targetGuildID {
			e := TimetableEntry{Title: r[2], Date: r[3]}
			if len(r) >= 6 {
				e.Memo, e.RemindRule = r[4], r[5]
			} else {
				e.RemindRule = r[4]
			}
			entries = append(entries, e)
		}
	}
	return entries
}

func deleteUserTimetableEntry(targetUserID, targetGuildID string, targetIndex int) error {
	f, _ := os.Open("timetable.csv")
	defer f.Close()
	records, _ := csv.NewReader(f).ReadAll()
	var newRecords [][]string
	userIndex := 0
	found := false
	for _, r := range records {
		if len(r) >= 5 && r[0] == targetUserID && r[1] == targetGuildID {
			userIndex++
			if userIndex == targetIndex {
				found = true
				continue
			}
		}
		newRecords = append(newRecords, r)
	}
	if !found {
		return fmt.Errorf("not found")
	}
	wFile, _ := os.OpenFile("timetable.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	defer wFile.Close()
	writer := csv.NewWriter(wFile)
	writer.WriteAll(newRecords)
	writer.Flush()
	return nil
}

func sendNotification(client bot.Client, channelID, userID, title, date, memo string) {
	msg := fmt.Sprintf("🔔 <@%s> **リマインド: 「%s」の予定**\n日付: %s", userID, title, date)
	if memo != "" {
		msg += "\nメモ: " + memo
	}
	client.Rest().CreateMessage(snowflakeID(channelID), discord.NewMessageCreateBuilder().SetContent(msg).Build())
}

func sendDM(client bot.Client, userID, title, date, memo string) {
	channel, _ := client.Rest().CreateDMChannel(snowflakeID(userID))
	msg := fmt.Sprintf("🔔 **リマインド: 「%s」の予定**\n日付: %s", title, date)
	if memo != "" {
		msg += "\nメモ: " + memo
	}
	client.Rest().CreateMessage(channel.ID(), discord.NewMessageCreateBuilder().SetContent(msg).Build())
}

func snowflakeID(id string) snowflake.ID {
	s, _ := snowflake.Parse(id)
	return s
}

type TimetableEntry struct {
	Title, Date, Memo, RemindRule string
}
