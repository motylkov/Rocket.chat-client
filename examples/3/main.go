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
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverURL := getenvOrDefault("ROCKET_SERVER_URL", defaultServerURL)
	userID := getenvOrDefault("ROCKET_USER_ID", defaultUserID)
	authToken := getenvOrDefault("ROCKET_AUTH_TOKEN", defaultAuthToken)
	roomIDs := splitAndTrim(os.Getenv("ROCKET_ROOM_IDS"))

	httpClient := &http.Client{Timeout: 15 * time.Second}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("starting bot, server=%s, static_rooms=%d", serverURL, len(roomIDs))

	bot := rocketbot.NewClient(rocketbot.Config{
		ServerURL:  serverURL,
		UserID:     userID,
		AuthToken:  authToken,
		HTTPClient: httpClient,
	})

	if err := bot.ValidateAuth(ctx); err != nil {
		log.Fatalf("auth validation failed: %v", err)
	}
	log.Printf("auth token validated")

	connectCtx, cancelConnect := context.WithTimeout(ctx, 20*time.Second)
	log.Printf("connecting to Rocket.Chat websocket...")
	if err := bot.Connect(connectCtx); err != nil {
		cancelConnect()
		log.Fatalf("connect failed: %v", err)
	}
	cancelConnect()
	log.Printf("connected successfully")
	defer bot.Close(websocket.StatusNormalClosure, "shutdown")

	if len(roomIDs) > 0 {
		log.Printf("using static room list from ROCKET_ROOM_IDS")
	} else {
		log.Printf("ROCKET_ROOM_IDS is empty, auto-discovering rooms via subscriptions API")
	}
	bot.StartAutoSubscribeRooms(ctx, rocketbot.AutoSubscribeOptions{
		StaticRoomIDs: roomIDs,
		Interval:      30 * time.Second,
		OnSubscribed: func(roomID string) {
			log.Printf("subscribed to room %s", roomID)
		},
		OnError: func(err error) {
			log.Printf("%v", err)
		},
		OnDiscovered: func(roomCount int) {
			log.Printf("auto-discovery found %d room(s)", roomCount)
		},
	})
	go heartbeat(ctx)

	bot.OnMessage(func(msgCtx context.Context, msg rocketbot.Message) {
		log.Printf("received message: text=%q room=%s sender=%s", msg.Text, msg.RoomID, msg.SenderID)
		// Не отвечаем сами себе, чтобы не зациклить echo.
		if msg.SenderID == userID {
			return
		}
		if err := bot.SendMessage(msgCtx, msg.RoomID, "echo: "+msg.Text); err != nil {
			log.Printf("send failed: %v", err)
		}
	})

	<-ctx.Done()
	log.Printf("shutdown signal received")
}

func splitAndTrim(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	raw := strings.Split(value, ",")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func getenvOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func heartbeat(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("bot is running")
		}
	}
}
