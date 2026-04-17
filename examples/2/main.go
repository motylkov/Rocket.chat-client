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
	defaultServerURL = "https://rocket.chat"
	defaultUserID    = "" // через переменную окружения
	defaultAuthToken = "" // через переменную окружения
	defaultRoomID    = "" // через переменную окружения (single-room mode)
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Чтение переменных окружения с fallback на пустые строки
	serverURL := getenvOrDefault("ROCKET_SERVER_URL", defaultServerURL)
	userID := getenvOrDefault("ROCKET_USER_ID", defaultUserID)
	authToken := getenvOrDefault("ROCKET_AUTH_TOKEN", defaultAuthToken)
	roomID := getenvOrDefault("ROCKET_ROOM_ID", defaultRoomID)

	// Проверка обязательных параметров
	if serverURL == "" {
		log.Fatal("ROCKET_SERVER_URL must be set")
	}
	if userID == "" || authToken == "" {
		log.Fatal("ROCKET_USER_ID and ROCKET_AUTH_TOKEN must be set")
	}
	if roomID == "" {
		log.Fatal("ROCKET_ROOM_ID is required for single-room mode")
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

	// Подписка на конкретную комнату
	if err := bot.SubscribeRoomMessages(ctx, roomID); err != nil {
		log.Fatalf("subscribe failed: %v", err)
	}
	log.Printf("subscribed to room %s", roomID)

	// Обработчик сообщений: отвечает всем (кроме самого себя) на команду "!status"
	bot.OnMessage(func(msgCtx context.Context, msg rocketbot.Message) {
		log.Printf("received message: text=%q room=%s sender=%s", msg.Text, msg.RoomID, msg.SenderID)

		// Не отвечаем сами себе
		if msg.SenderID == userID {
			return
		}
		// Реагируем только на точную команду "!status"
		if strings.TrimSpace(msg.Text) != "!status" {
			return
		}

		// Отдельный контекст с таймаутом для отправки ответа
		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := bot.SendMessage(sendCtx, msg.RoomID, "Все хорошо"); err != nil {
			log.Printf("send failed: %v", err)
		} else {
			log.Printf("sent status reply to room %s", msg.RoomID)
		}
	})

	log.Println("bot is running in single-room mode, waiting for shutdown signal...")
	<-ctx.Done()

	// Graceful shutdown: даём время на завершение активных операций
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
