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

// Значения по умолчанию
const (
	defaultServerURL       = "https://rocket.chat"
	defaultUserID          = "" // через переменную окружения
	defaultAuthToken       = "" // через переменную окружения
	defaultAllowedSenderID = "" // через переменную окружения
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverURL := getenvOrDefault("ROCKET_SERVER_URL", defaultServerURL)
	userID := getenvOrDefault("ROCKET_USER_ID", defaultUserID)
	authToken := getenvOrDefault("ROCKET_AUTH_TOKEN", defaultAuthToken)
	roomIDs := splitAndTrim(os.Getenv("ROCKET_ROOM_IDS"))
	allowedSenderID := getenvOrDefault("ROCKET_ALLOWED_SENDER_ID", defaultAllowedSenderID)

	if serverURL == "" || userID == "" || authToken == "" {
		log.Fatal("ROCKET_SERVER_URL, ROCKET_USER_ID, and ROCKET_AUTH_TOKEN must be set")
	}
	if allowedSenderID == "" {
		log.Fatal("ROCKET_ALLOWED_SENDER_ID must be set: bot will respond only to that user")
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
	if err := bot.Connect(connectCtx); err != nil {
		cancelConnect()
		log.Fatalf("connect failed: %v", err)
	}
	cancelConnect()
	defer bot.Close(websocket.StatusNormalClosure, "shutdown")

	bot.StartAutoSubscribeRooms(ctx, rocketbot.AutoSubscribeOptions{
		StaticRoomIDs: roomIDs,
		Interval:      30 * time.Second,
		OnSubscribed: func(roomID string) {
			log.Printf("subscribed to room %s", roomID)
		},
		OnError: func(err error) {
			log.Printf("auto-subscribe error: %v", err)
		},
	})

	// Обработчик сообщений: отвечает только разрешенному пользователю на команду "!status"
	bot.OnMessage(func(msgCtx context.Context, msg rocketbot.Message) {
		log.Printf("received message: text=%q room=%s sender=%s", msg.Text, msg.RoomID, msg.SenderID)

		// Не отвечаем сами себе
		if msg.SenderID == userID {
			return
		}
		// Отвечаем только разрешенному пользователю
		if msg.SenderID != allowedSenderID {
			log.Printf("ignoring message from %s (allowed only %s)", msg.SenderID, allowedSenderID)
			return
		}
		// Реагируем только на точную команду "!status"
		if strings.TrimSpace(msg.Text) != "!status" {
			return
		}

		// Формируем attachment с таблицей статуса
		attachment := rocketbot.Attachment{
			Title: "📊 Server Status Table",
			Text:  "Current health metrics",
			Color: "#2de0a5",
			Fields: []rocketbot.AttachmentField{
				{Title: "Server", Value: "web-01\nweb-02\ndb-01", Short: true},
				{Title: "CPU %", Value: "12\n45\n8", Short: true},
				{Title: "Memory %", Value: "34\n72\n21", Short: true},
				{Title: "Status", Value: "🟢 OK\n🟡 Warning\n🟢 OK", Short: true},
			},
			Ts: time.Now(),
		}

		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := bot.SendMessageWithAttachment(sendCtx, msg.RoomID, attachment); err != nil {
			log.Printf("failed to send table: %v", err)
		} else {
			log.Printf("sent status table to room %s for user %s", msg.RoomID, msg.SenderID)
		}
	})

	<-ctx.Done()
	log.Println("shutting down...")
	time.Sleep(2 * time.Second)
}

// splitAndTrim разбивает строку по запятым и обрезает пробелы, возвращает список непустых room ID.
func splitAndTrim(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// getenvOrDefault возвращает значение переменной окружения или fallback, если оно пустое.
func getenvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
