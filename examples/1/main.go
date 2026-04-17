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
	defaultRoomID          = "" // опционально
	defaultAllowedSenderID = "" // allowedSenderID - бот будет отвечать ТОЛЬКО ему
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Чтение переменных окружения с fallback на значения по умолчанию
	serverURL := getenvOrDefault("ROCKET_SERVER_URL", defaultServerURL)
	userID := getenvOrDefault("ROCKET_USER_ID", defaultUserID)
	authToken := getenvOrDefault("ROCKET_AUTH_TOKEN", defaultAuthToken)
	roomID := getenvOrDefault("ROCKET_ROOM_ID", defaultRoomID)
	allowedSenderID := getenvOrDefault("ROCKET_ALLOWED_SENDER_ID", defaultAllowedSenderID)

	// Проверка обязательных параметров
	if userID == "" || authToken == "" {
		log.Fatal("ROCKET_USER_ID and ROCKET_AUTH_TOKEN must be set")
	}
	if allowedSenderID == "" {
		log.Fatal("ROCKET_ALLOWED_SENDER_ID must be set: bot will respond only to that user")
	}
	if serverURL == "" {
		log.Fatal("ROCKET_SERVER_URL must be set")
	}

	// Создание клиента
	bot := rocketbot.NewClient(rocketbot.Config{
		ServerURL:  serverURL,
		UserID:     userID,
		AuthToken:  authToken,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	})

	// Проверка аутентификации перед подключением
	if err := bot.ValidateAuth(ctx); err != nil {
		log.Fatalf("auth validation failed: %v", err)
	}

	// Подключение с таймаутом
	connectCtx, cancelConnect := context.WithTimeout(ctx, 20*time.Second)
	defer cancelConnect()
	if err := bot.Connect(connectCtx); err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer bot.Close(websocket.StatusNormalClosure, "shutdown")

	// Подписка на комнату (если указана) или авто-подписка
	if roomID != "" {
		if err := bot.SubscribeRoomMessages(ctx, roomID); err != nil {
			log.Fatalf("subscribe failed: %v", err)
		}
		log.Printf("subscribed to room %s", roomID)
	} else {
		log.Printf("ROCKET_ROOM_ID is empty, auto-discovering rooms")
		bot.StartAutoSubscribeRooms(ctx, rocketbot.AutoSubscribeOptions{
			Interval: 30 * time.Second,
			OnSubscribed: func(discoveredRoomID string) {
				log.Printf("subscribed to room %s", discoveredRoomID)
			},
			OnError: func(err error) {
				log.Printf("auto-subscribe error: %v", err)
			},
		})
	}

	// Обработчик сообщений: отвечает ТОЛЬКО пользователю allowedSenderID
	bot.OnMessage(func(msgCtx context.Context, msg rocketbot.Message) {
		log.Printf("received message: text=%q room=%s sender=%s", msg.Text, msg.RoomID, msg.SenderID)

		// Не отвечаем сами себе (на случай, если бот случайно сгенерирует сообщение)
		if msg.SenderID == userID {
			return
		}
		// Отвечаем только разрешённому пользователю
		if msg.SenderID != allowedSenderID {
			log.Printf("ignoring message from %s (allowed only %s)", msg.SenderID, allowedSenderID)
			return
		}

		// Отдельный контекст с таймаутом для отправки сообщения
		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := bot.SendMessage(sendCtx, msg.RoomID, "echo: "+msg.Text); err != nil {
			log.Printf("send failed: %v", err)
		} else {
			log.Printf("sent echo to room %s", msg.RoomID)
		}
	})

	log.Println("bot is running, waiting for shutdown signal...")
	<-ctx.Done()

	// Даём время на завершение активных операций (например, отправки последнего сообщения)
	log.Println("shutting down gracefully...")
	time.Sleep(2 * time.Second)
	log.Println("bot stopped")
}

// getenvOrDefault возвращает значение переменной окружения или fallback, если оно пустое (после обрезки пробелов)
func getenvOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
