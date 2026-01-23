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
	StepTitle          = 1
	StepDate           = 2
	StepRemindSettings = 3
	StepSetChannel     = 10
)

type UserSession struct {
	Step    int
	Title   string // タイトル一時保存用
	DateStr string // 日付一時保存用
}

var sessions = make(map[string]*UserSession)
var mu sync.Mutex

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("could not find .env file")
	}

	token := os.Getenv("DISCORD_BOT_TOKEN")
	prefix := "!"

	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildMessages,
				gateway.IntentMessageContent,
				gateway.IntentDirectMessages,
			),
		),
		bot.WithEventListenerFunc(func(e *events.MessageCreate) {
			if e.Message.Author.Bot {
				return
			}

			userID := e.Message.Author.ID.String()

			mu.Lock()
			session, exists := sessions[userID]
			mu.Unlock()

			// --- 会話モードの処理 ---
			if exists {
				switch session.Step {
				// Step 1: タイトル入力 -> 日付入力へ
				case StepTitle:
					title := e.Message.Content
					mu.Lock()
					sessions[userID].Title = title
					sessions[userID].Step = StepDate
					mu.Unlock()
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("日付を入力してください 例:yyyy/mm/dd").Build())
					return

				// Step 2: 日付入力 -> リマインド設定へ
				case StepDate:
					dateStr := strings.TrimSpace(e.Message.Content)
					_, parseErr := time.Parse("2006/01/02", dateStr)

					if parseErr != nil {
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("有効なフォーマットの日付を入力してください (例: 2025/12/29)").Build())
						return
					}

					mu.Lock()
					sessions[userID].DateStr = dateStr
					sessions[userID].Step = StepRemindSettings
					mu.Unlock()

					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("何時間前、あるいは何日前にリマインドを送りますか？\n(例: '1d' = 1日前, '12h' = 12時間前, '0' = 当日通知)").Build())
					return

				// Step 3: リマインド設定 -> 保存
				case StepRemindSettings:
					setting := strings.TrimSpace(e.Message.Content)

					// 入力が解析可能かチェック
					duration, err := parseCustomDuration(setting)
					if err != nil {
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("形式が正しくありません。例: '1d', '3h', '30m', '0'").Build())
						return
					}

					// 確認メッセージ作成
					offsetStr := duration.String()
					if strings.HasSuffix(setting, "d") {
						offsetStr = setting // "1d" などの表示を優先
					}

					saveErr := saveToCSV(userID, session.Title, session.DateStr, setting)
					if saveErr != nil {
						log.Println("CSV error:", saveErr)
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("保存中にエラーが発生しました。").Build())
					} else {
						msg := fmt.Sprintf("タイムテーブルを保存しました。\n予定: %s (%s)\n通知: %s 前", session.Title, session.DateStr, offsetStr)
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent(msg).Build())
					}

					mu.Lock()
					delete(sessions, userID)
					mu.Unlock()
					return

				// Step 10: チャンネル設定の処理
				case StepSetChannel:
					input := strings.TrimSpace(e.Message.Content)

					// チャンネルメンション形式 (<#数字>) かどうかチェック
					if strings.HasPrefix(input, "<#") && strings.HasSuffix(input, ">") {
						// IDだけ抽出 ( <# と > を削除)
						channelID := input[2 : len(input)-1]

						// チャンネル設定を保存
						err := saveChannelConfig(userID, channelID)
						if err != nil {
							e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("設定の保存に失敗しました").Build())
						} else {
							// Discord上では <#ID> と送ると自動的に「#チャンネル名」というリンク表示になります
							msg := fmt.Sprintf("通知を <#%s> に設定しました", channelID)
							e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent(msg).Build())
						}
					} else {
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("チャンネルを正しくメンションしてください (例: #雑談チャンネル)").Build())
					}

					// セッション終了
					mu.Lock()
					delete(sessions, userID)
					mu.Unlock()
					return
				}
			}

			// --- コマンド判定 ---
			if !strings.HasPrefix(e.Message.Content, prefix) {
				return
			}

			cmd := strings.TrimPrefix(e.Message.Content, prefix)

			if cmd == "timetable" {
				// チャンネル設定の事前チェック
				configs := loadChannelConfigs()
				if _, ok := configs[userID]; !ok {
					// 設定がない場合
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("!setchで通知の設定をしてください").Build())
					return
				}

				// 設定がある場合は通常通り開始
				mu.Lock()
				sessions[userID] = &UserSession{Step: StepTitle}
				mu.Unlock()
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("タイトルを入力してください").Build())

			} else if cmd == "setch" {
				mu.Lock()
				sessions[userID] = &UserSession{Step: StepSetChannel}
				mu.Unlock()
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("通知先のチャンネルを設定してください (例: #雑談チャンネル)").Build())

			} else if cmd == "testremind" {
				checkAndSendReminders(e.Client())
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("リマインド確認処理を実行しました").Build())

			} else if cmd == "tablelist" {
				timetables := getUserTimetables(userID)
				if len(timetables) == 0 {
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("登録されている予定はありません。").Build())
				} else {
					var sb strings.Builder
					sb.WriteString("📋 **あなたの予定一覧**\n")
					for i, t := range timetables {
						// 1-based index for user friendliness
						sb.WriteString(fmt.Sprintf("%d. %s (%s) - 通知: %s\n", i+1, t.Title, t.Date, t.RemindRule))
					}
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent(sb.String()).Build())
				}

			} else if strings.HasPrefix(cmd, "delete") {
				// !delete 1
				args := strings.Fields(cmd)
				if len(args) < 2 {
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("削除する番号を指定してください (例: !delete 1)").Build())
					return
				}

				var index int
				_, err := fmt.Sscanf(args[1], "%d", &index)
				if err != nil || index < 1 {
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("有効な番号を指定してください").Build())
					return
				}

				err = deleteUserTimetableEntry(userID, index)
				if err != nil {
					log.Println("Delete error:", err)
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("削除に失敗しました").Build())
				} else {
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("予定を削除しました").Build())
				}
			}
		}),
	)
	if err != nil {
		panic(err)
	}

	if err = client.OpenGateway(context.TODO()); err != nil {
		panic(err)
	}

	go startReminderLoop(client)

	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
	<-s
}

// "1d" などの日数を time.Duration に変換するヘルパー
func parseCustomDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	// "d" が含まれていれば 24h に置換して parse
	if strings.HasSuffix(s, "d") {
		daysStr := strings.TrimSuffix(s, "d")
		// 単純に数値 * 24h を計算する方が確実
		var d float64
		_, err := fmt.Sscanf(daysStr, "%f", &d)
		if err != nil {
			return 0, err
		}
		return time.Duration(d * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func saveToCSV(userID, title, date, remindRule string) error {
	f, err := os.OpenFile("timetable.csv", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := csv.NewWriter(f)
	defer writer.Flush()
	return writer.Write([]string{userID, title, date, remindRule})
}

func saveChannelConfig(userID, channelID string) error {
	f, err := os.OpenFile("channels.csv", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := csv.NewWriter(f)
	defer writer.Flush()
	return writer.Write([]string{userID, channelID})
}

func startReminderLoop(client bot.Client) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		// 毎分チェック
		checkAndSendReminders(client)
	}
}

func checkAndSendReminders(client bot.Client) {
	// 1. タイムテーブル読み込み
	f, err := os.Open("timetable.csv")
	if err != nil {
		return
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1 // カラム数が可変であることを許容
	records, err := reader.ReadAll()
	if err != nil {
		return
	}

	// 現在時刻
	now := time.Now()

	// 2. チャンネル設定読み込み
	channelMap := loadChannelConfigs()

	for _, record := range records {
		// カラム数が足りない古いデータはスキップまたはデフォルト値で処理
		if len(record) < 3 {
			continue
		}

		userID := record[0]
		title := record[1]
		dateStr := record[2]

		remindRule := "1d" // デフォルト: 1日前
		if len(record) >= 4 {
			remindRule = record[3]
		}

		// 予定されている日付をパース
		targetDate, err := time.Parse("2006/01/02", dateStr)
		if err != nil {
			continue
		}

		// 基準時間を「当日の朝9時」とする (例: 2026/01/24 09:00:00)
		eventTime := time.Date(targetDate.Year(), targetDate.Month(), targetDate.Day(), 9, 0, 0, 0, time.Local)

		// リマインドタイミングを計算
		duration, err := parseCustomDuration(remindRule)
		if err != nil {
			continue
		}

		// 通知すべき時間 = イベント時間 - 指定時間
		notifyTime := eventTime.Add(-duration)

		// 現在時刻が通知時刻と一致するかチェック (精度: 分)
		// diff が -30秒 < diff < 30秒 であれば送る
		diff := now.Sub(notifyTime)
		if diff >= -35*time.Second && diff < 35*time.Second {
			targetChannelID, ok := channelMap[userID]
			if ok {
				sendNotification(client, targetChannelID, userID, title, dateStr)
			} else {
				sendDM(client, userID, title, dateStr)
			}
		}
	}
}

func loadChannelConfigs() map[string]string {
	m := make(map[string]string)
	f, err := os.Open("channels.csv")
	if err != nil {
		return m
	}
	defer f.Close()

	reader := csv.NewReader(f)
	records, _ := reader.ReadAll()

	for _, row := range records {
		if len(row) >= 2 {
			m[row[0]] = row[1]
		}
	}
	return m
}

func sendNotification(client bot.Client, channelID, userID, title, date string) {
	msg := fmt.Sprintf("🔔 <@%s> **リマインド: 「%s」の予定が近づいています**\n日付: %s\n準備はできていますか？", userID, title, date)
	client.Rest().CreateMessage(snowflakeID(channelID), discord.NewMessageCreateBuilder().SetContent(msg).Build())
	log.Printf("Sent channel reminder to %s for %s", channelID, title)
}

func sendDM(client bot.Client, userID, title, date string) {
	channel, err := client.Rest().CreateDMChannel(snowflakeID(userID))
	if err != nil {
		log.Println("DM作成エラー:", err)
		return
	}
	msg := fmt.Sprintf("🔔 **リマインド: 「%s」の予定が近づいています**\n日付: %s", title, date)
	client.Rest().CreateMessage(channel.ID(), discord.NewMessageCreateBuilder().SetContent(msg).Build())
}

func snowflakeID(id string) snowflake.ID {
	s, _ := snowflake.Parse(id)
	return s
}

type TimetableEntry struct {
	Title      string
	Date       string
	RemindRule string
}

func getUserTimetables(targetUserID string) []TimetableEntry {
	var entries []TimetableEntry
	f, err := os.Open("timetable.csv")
	if err != nil {
		return entries
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1 // カラム数が可変であることを許容
	records, err := reader.ReadAll()
	if err != nil {
		return entries
	}

	for _, record := range records {
		if len(record) < 3 {
			continue
		}
		// userIDが一致するものだけ
		if record[0] == targetUserID {
			remindRule := "1d"
			if len(record) >= 4 {
				remindRule = record[3]
			}
			entries = append(entries, TimetableEntry{
				Title:      record[1],
				Date:       record[2],
				RemindRule: remindRule,
			})
		}
	}
	return entries
}

func deleteUserTimetableEntry(targetUserID string, targetIndex int) error {
	// 1. Read all records
	f, err := os.Open("timetable.csv")
	if err != nil {
		return err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return err
	}

	// 2. Filter records
	var newRecords [][]string
	userIndex := 0 // Counter for this user's records

	for _, record := range records {
		if len(record) < 3 {
			continue
		}

		if record[0] == targetUserID {
			userIndex++
			// If this matches the target index, skip it (delete it)
			if userIndex == targetIndex {
				continue
			}
		}
		newRecords = append(newRecords, record)
	}

	// 3. Write back to file (overwrite)
	// Open with O_TRUNC to clear file content before writing
	wFile, err := os.OpenFile("timetable.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer wFile.Close()

	writer := csv.NewWriter(wFile)
	defer writer.Flush()

	return writer.WriteAll(newRecords)
}
