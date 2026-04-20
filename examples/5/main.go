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
	drumImagePath := getenvOrDefault("DRUM_IMAGE_PATH", "drum.png")

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

	// Обработчик: отвечает на !status только разрешённому пользователю
	bot.OnMessage(func(msgCtx context.Context, msg rocketbot.Message) {
		// Не отвечаем сами себе
		if msg.SenderID == userID {
			return
		}
		// Отвечаем только заданному пользователю
		if msg.SenderID != allowedSenderID {
			log.Printf("ignoring message from %s (allowed only %s)", msg.SenderID, allowedSenderID)
			return
		}
		// Проверяем команду
		if strings.TrimSpace(msg.Text) != "!status" {
			return
		}

		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		uploadResult, err := bot.UploadRoomFileDetailed(sendCtx, msg.RoomID, drumImagePath, "drum")
		if err != nil {
			log.Printf("failed to send image: %v", err)
		} else {
			log.Printf("sent image %q to room %s for user %s, message_id=%q", drumImagePath, msg.RoomID, msg.SenderID, uploadResult.MessageID)
			if err := bot.SendMessage(sendCtx, msg.RoomID, "debug: image upload API returned success"); err != nil {
				log.Printf("failed to send debug confirmation: %v", err)
			}
		}
	})

	<-ctx.Done()
	log.Println("shutting down...")
	time.Sleep(2 * time.Second)
}

// --- Утилиты ---
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

func getenvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
