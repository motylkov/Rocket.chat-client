package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/coder/websocket"
	rocketbot "github.com/motylkov/Rocket.chat-client"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	roomID := os.Getenv("ROCKET_ROOM_ID")

	bot := rocketbot.NewClient(rocketbot.Config{
		ServerURL: os.Getenv("ROCKET_SERVER_URL"),
		UserID:    os.Getenv("ROCKET_USER_ID"),
		AuthToken: os.Getenv("ROCKET_AUTH_TOKEN"),
	})

	if err := bot.Connect(ctx); err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer bot.Close(websocket.StatusNormalClosure, "shutdown")

	if err := bot.SubscribeRoomMessages(ctx, roomID); err != nil {
		log.Fatalf("subscribe failed: %v", err)
	}

	bot.OnMessage(func(msgCtx context.Context, msg rocketbot.Message) {
		// Не отвечаем сами себе, чтобы не зациклить echo.
		if msg.SenderID == os.Getenv("ROCKET_USER_ID") {
			return
		}
		if err := bot.SendMessage(msgCtx, msg.RoomID, "echo: "+msg.Text); err != nil {
			log.Printf("send failed: %v", err)
		}
	})

	<-ctx.Done()
}
