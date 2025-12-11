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

type UserSession struct {
	Step  int    // 1: タイトル入力, 2: 日付入力, 3: チャンネル設定入力
	Title string // タイトル一時保存用
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
				// Step 1~2: タイムテーブル登録
				case 1:
					title := e.Message.Content
					mu.Lock()
					sessions[userID].Title = title
					sessions[userID].Step = 2
					mu.Unlock()
					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("日付を入力してください 例:yyyy/mm/dd").Build())
					return

				case 2:
					dateStr := strings.TrimSpace(e.Message.Content)
					_, parseErr := time.Parse("2006/01/02", dateStr)

					if parseErr != nil {
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("有効なフォーマットの日付を入力してください (例: 2025/12/29)").Build())
						return
					}

					err := saveToCSV(userID, session.Title, dateStr)
					if err != nil {
						log.Println("CSV error:", err)
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("保存中にエラーが発生しました。").Build())
					} else {
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("タイムテーブルを保存しました").Build())
					}

					mu.Lock()
					delete(sessions, userID)
					mu.Unlock()
					return

				// Step 3: チャンネル設定の処理
				case 3:
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
				mu.Lock()
				sessions[userID] = &UserSession{Step: 1}
				mu.Unlock()
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("タイトルを入力してください").Build())

			} else if cmd == "setch" {
				mu.Lock()
				sessions[userID] = &UserSession{Step: 3} // Step 3はチャンネル設定モード
				mu.Unlock()
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("通知先のチャンネルを設定してください (例: #雑談チャンネル)").Build())
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

func saveToCSV(userID, title, date string) error {
	f, err := os.OpenFile("timetable.csv", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := csv.NewWriter(f)
	defer writer.Flush()
	return writer.Write([]string{userID, title, date})
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
		now := time.Now()
		// 動作テスト用: 毎分0秒にチェック
		if now.Second() == 0 {
			// 本番運用時はこちらを使ってください:
			// if now.Hour() == 9 && now.Minute() == 0 {
			checkAndSendReminders(client)
		}
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
	records, err := reader.ReadAll()
	if err != nil {
		return
	}

	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006/01/02")

	// 2. チャンネル設定読み込み
	channelMap := loadChannelConfigs()

	for _, record := range records {
		if len(record) < 3 {
			continue
		}
		userID := record[0]
		title := record[1]
		date := record[2]

		if date == tomorrow {
			targetChannelID, ok := channelMap[userID]

			if ok {
				// 設定があればそのチャンネルへ送信
				sendNotification(client, targetChannelID, userID, title, date)
			} else {
				// 設定がなければDMへ送信
				sendDM(client, userID, title, date)
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
	msg := fmt.Sprintf("🔔 <@%s> **リマインド: 明日は「%s」の予定があります**\n日付: %s\n準備はできていますか？", userID, title, date)
	client.Rest().CreateMessage(snowflakeID(channelID), discord.NewMessageCreateBuilder().SetContent(msg).Build())
	log.Printf("Sent channel reminder to %s for %s", channelID, title)
}

func sendDM(client bot.Client, userID, title, date string) {
	channel, err := client.Rest().CreateDMChannel(snowflakeID(userID))
	if err != nil {
		log.Println("DM作成エラー:", err)
		return
	}
	msg := fmt.Sprintf("🔔 **リマインド: 明日は「%s」の予定があります**\n日付: %s", title, date)

	// ★修正箇所: channel.ID を channel.ID() に変更しました
	client.Rest().CreateMessage(channel.ID(), discord.NewMessageCreateBuilder().SetContent(msg).Build())
}

func snowflakeID(id string) snowflake.ID {
	s, _ := snowflake.Parse(id)
	return s
}
