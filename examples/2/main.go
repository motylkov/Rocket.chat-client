package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	rocketbot "github.com/motylkov/Rocket.chat-client"
)

var (
	defaultServerURL = "https://rocket.hostname"
	defaultUserID    = "xxxxxxxxxxx"
	defaultAuthToken = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	defaultRoomID    = "xxxxxxxxxxxxxxxxxxxxxx" // Set room id for a single target group; can be overridden by ROCKET_ROOM_ID.

)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverURL := getenvOrDefault("ROCKET_SERVER_URL", defaultServerURL)
	userID := getenvOrDefault("ROCKET_USER_ID", defaultUserID)
	authToken := getenvOrDefault("ROCKET_AUTH_TOKEN", defaultAuthToken)
	roomID := getenvOrDefault("ROCKET_ROOM_ID", defaultRoomID)
	if roomID == "" {
		log.Fatal("ROCKET_ROOM_ID is required for single-room mode")
	}

	bot := rocketbot.NewClient(rocketbot.Config{
		ServerURL:  serverURL,
		UserID:     userID,
		AuthToken:  authToken,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	})

	if err := bot.ValidateAuth(ctx); err != nil {
		log.Fatalf("auth validation failed: %v", err)
	}

	connectCtx, cancelConnect := context.WithTimeout(ctx, 20*time.Second)
	defer cancelConnect()
	if err := bot.Connect(connectCtx); err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer bot.Close(websocket.StatusNormalClosure, "shutdown")

	if err := bot.SubscribeRoomMessages(ctx, roomID); err != nil {
		log.Fatalf("subscribe failed: %v", err)
	}
	log.Printf("subscribed to room %s", roomID)

	bot.OnMessage(func(msgCtx context.Context, msg rocketbot.Message) {
		log.Printf("received message: text=%q room=%s sender=%s", msg.Text, msg.RoomID, msg.SenderID)
		// Не отвечаем сами себе, чтобы не зациклить echo.
		if msg.SenderID == userID {
			return
		}
		if strings.TrimSpace(msg.Text) != "!status" {
			return
		}
		if err := bot.SendMessage(msgCtx, msg.RoomID, "Все хорошо"); err != nil {
			log.Printf("send failed: %v", err)
		}
	})

	<-ctx.Done()
}

func getenvOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
