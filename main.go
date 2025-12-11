package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/joho/godotenv"
)

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
			if e.Message.Author.Bot {
				return
			}
			// メッセージが ":" で始まっていなければ何もしない
			if !strings.HasPrefix(e.Message.Content, prefix) {
				return
			}

			// 3. プレフィックスを削除して中身を取り出す (例: ":ping" -> "ping")
			cmd := strings.TrimPrefix(e.Message.Content, prefix)

			// 4. コマンドに応じた処理
			if cmd == "ping" {
				e.Client().Rest().CreateMessage(e.ChannelID, discord.NewMessageCreateBuilder().SetContent("pong").Build())
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
