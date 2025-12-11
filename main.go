package main

import (
	"context"
	"encoding/csv"
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
	Step  int    // 1: タイトル入力待ち, 2: 日付入力待ち
	Title string // 入力されたタイトルを一時保存
}

// ユーザーごとのセッションを保存するマップ (key: UserID)
var sessions = make(map[string]*UserSession)
var mu sync.Mutex // マップの同時アクセスを防ぐロック

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("could not find .env file")
	}

	token := os.Getenv("DISCORD_BOT_TOKEN")
	prefix := "!"

	client, err := disgo.New(token,
		// set gateway options
		bot.WithGatewayConfigOpts(
			// set enabled intents
			gateway.WithIntents(
				gateway.IntentGuilds,
				gateway.IntentGuildMessages,
				gateway.IntentMessageContent,
			),
		),
		// add event listeners
		bot.WithEventListenerFunc(func(e *events.MessageCreate) {
			// 自分のメッセージは無視
			if e.Message.Author.Bot {
				return
			}

			userID := e.Message.Author.ID.String()

			mu.Lock()
			session, exists := sessions[userID]
			mu.Unlock()

			// --- 会話中の処理 ---
			if exists {
				switch session.Step {
				case 1: // タイトル入力待ちの状態
					title := e.Message.Content

					// タイトルを保存して次のステップへ
					mu.Lock()
					sessions[userID].Title = title
					sessions[userID].Step = 2
					mu.Unlock()

					e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("日付を入力してください 例:yyyy/mm/dd").Build())
					return

				case 2: // 日付入力待ちの状態
					// 前後の空白を削除しておく
					dateStr := strings.TrimSpace(e.Message.Content)

					// 日付フォーマットのチェック
					_, parseErr := time.Parse("2006/01/02", dateStr)

					if parseErr != nil {
						// フォーマット不正時の例外処理
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("有効なフォーマットの日付を入力してください (例: 2025/12/29)").Build())
						return // ステップは進めず、もう一度入力させる
					}

					// CSVに保存
					err := saveToCSV(userID, session.Title, dateStr)
					if err != nil {
						log.Println("CSV error:", err)
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("保存中にエラーが発生しました。").Build())
					} else {
						e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("タイムテーブルを保存しました").Build())
					}

					// 会話終了なのでセッションを削除
					mu.Lock()
					delete(sessions, userID)
					mu.Unlock()
					return
				}
			}

			// --- 新規コマンドの開始処理 ---

			// メッセージが "!" で始まっていなければ何もしない
			if !strings.HasPrefix(e.Message.Content, prefix) {
				return
			}

			// プレフィックスを削除して中身を取り出す (例: "!timetable" -> "timetable")
			cmd := strings.TrimPrefix(e.Message.Content, prefix)

			if cmd == "timetable" {
				// 新しいセッションを開始
				mu.Lock()
				sessions[userID] = &UserSession{
					Step: 1, // タイトル入力待ちからスタート
				}
				mu.Unlock()

				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("タイトルを入力してください").Build())
			}
		}),
	)
	if err != nil {
		panic(err)
	}
	// connect to the gateway
	if err = client.OpenGateway(context.TODO()); err != nil {
		panic(err)
	}

	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
	<-s
}

// CSVに書き込む関数
func saveToCSV(userID, title, date string) error {
	// 追記モードでファイルを開く。なければ作成。
	f, err := os.OpenFile("timetable.csv", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	writer := csv.NewWriter(f)
	defer writer.Flush()

	// データを書き込み (UserID, Title, Date)
	return writer.Write([]string{userID, title, date})
}
