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
	"github.com/joho/godotenv"
)

type UserSession struct {
	Step  int
	Title string
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
				gateway.IntentDirectMessages, // DMを送るために必要になることがあります
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

			if exists {
				switch session.Step {
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
				}
			}

			if !strings.HasPrefix(e.Message.Content, prefix) {
				return
			}

			cmd := strings.TrimPrefix(e.Message.Content, prefix)

			if cmd == "timetable" {
				mu.Lock()
				sessions[userID] = &UserSession{
					Step: 1,
				}
				mu.Unlock()
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("タイトルを入力してください").Build())
			}
		}),
	)
	if err != nil {
		panic(err)
	}

	if err = client.OpenGateway(context.TODO()); err != nil {
		panic(err)
	}

	// ★ここでリマインダー監視ループを起動！ (並行処理)
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

// ★リマインダー機能の本体
func startReminderLoop(client bot.Client) {
	// 1分ごとにチェックを行う
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()

		// 毎日 朝9時00分 にチェックして通知を送る設定
		// (動作テストするときは、ここの時間を現在の時刻+1分とかにして試してください)
		if now.Hour() == 9 && now.Minute() == 0 {
			checkAndSendReminders(client)
		}
	}
}

func checkAndSendReminders(client bot.Client) {
	f, err := os.Open("timetable.csv")
	if err != nil {
		// ファイルがない場合は何もしない
		return
	}
	defer f.Close()

	reader := csv.NewReader(f)
	records, err := reader.ReadAll()
	if err != nil {
		log.Println("CSV read error:", err)
		return
	}

	// 「明日」の日付文字列を作る (例: 今日が12/10なら "2025/12/11")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006/01/02")

	for _, record := range records {
		if len(record) < 3 {
			continue
		}
		userID := record[0]
		title := record[1]
		date := record[2]

		// CSVの日付が「明日」と一致するか？
		if date == tomorrow {
			sendDM(client, userID, title, date)
		}
	}
}

func sendDM(client bot.Client, userID, title, date string) {
	// DMチャンネルを作成 (まだ無ければ作成される)
	channel, err := client.Rest().CreateDMUser(snowflake(userID))
	if err != nil {
		log.Println("DM作成エラー:", err)
		return
	}

	msg := fmt.Sprintf("🔔 **リマインド: 明日は「%s」の予定があります**\n日付: %s\n準備はできていますか？", title, date)

	client.Rest().CreateMessage(channel.ID, discord.NewMessageCreateBuilder().SetContent(msg).Build())
	log.Printf("Sent reminder to %s for %s", userID, title)
}

// 文字列IDをSnowflake型に変換するヘルパー関数
func snowflake(id string) discord.Snowflake {
	s, _ := discord.ParseSnowflake(id)
	return s
}
