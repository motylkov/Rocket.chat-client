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

// Константы по умолчанию
const (
	defaultServerURL = "https://rocket.chat"
	defaultUserID    = "" // через переменную окружения
	defaultAuthToken = "" // через переменную окружения)
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Чтение переменных окружения
	serverURL := getenvOrDefault("ROCKET_SERVER_URL", defaultServerURL)
	userID := getenvOrDefault("ROCKET_USER_ID", defaultUserID)
	authToken := getenvOrDefault("ROCKET_AUTH_TOKEN", defaultAuthToken)
	roomIDsEnv := os.Getenv("ROCKET_ROOM_IDS")
	roomIDs := splitAndTrim(roomIDsEnv) // может быть пустым — тогда автообнаружение

	// Проверка обязательных параметров
	if serverURL == "" {
		log.Fatal("ROCKET_SERVER_URL must be set")
	}
	if userID == "" || authToken == "" {
		log.Fatal("ROCKET_USER_ID and ROCKET_AUTH_TOKEN must be set")
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("starting bot, server=%s, static_rooms=%d", serverURL, len(roomIDs))

	bot := rocketbot.NewClient(rocketbot.Config{
		ServerURL:  serverURL,
		UserID:     userID,
		AuthToken:  authToken,
		HTTPClient: httpClient,
	})

	// Проверка аутентификации перед подключением
	if err := bot.ValidateAuth(ctx); err != nil {
		log.Fatalf("auth validation failed: %v", err)
	}
	log.Printf("auth token validated")

	// Подключение с таймаутом
	connectCtx, cancelConnect := context.WithTimeout(ctx, 20*time.Second)
	log.Printf("connecting to Rocket.Chat websocket...")
	if err := bot.Connect(connectCtx); err != nil {
		cancelConnect()
		log.Fatalf("connect failed: %v", err)
	}
	cancelConnect()
	log.Printf("connected successfully")
	defer bot.Close(websocket.StatusNormalClosure, "shutdown")

	// Настройка авто-подписки (со статическими комнатами, если указаны)
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
			log.Printf("auto-subscribe error: %v", err)
		},
		OnDiscovered: func(roomCount int) {
			log.Printf("auto-discovery found %d room(s)", roomCount)
		},
	})

	// Запуск фонового heartbeat для мониторинга
	go heartbeat(ctx)

	// Обработчик сообщений: отвечает всем (кроме себя) эхом
	bot.OnMessage(func(msgCtx context.Context, msg rocketbot.Message) {
		log.Printf("received message: text=%q room=%s sender=%s", msg.Text, msg.RoomID, msg.SenderID)

		// Не отвечаем сами себе
		if msg.SenderID == userID {
			return
		}

		// Отдельный контекст с таймаутом для отправки ответа
		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := bot.SendMessage(sendCtx, msg.RoomID, "echo: "+msg.Text); err != nil {
			log.Printf("send failed: %v", err)
		} else {
			log.Printf("sent echo to room %s", msg.RoomID)
		}
	})

	<-ctx.Done()
	log.Printf("shutdown signal received, waiting for pending operations...")
	time.Sleep(2 * time.Second) // Даём время на отправку последних сообщений
	log.Printf("bot stopped")
}

// splitAndTrim разбивает строку по запятым и обрезает пробелы, возвращает список непустых ID комнат.
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

// getenvOrDefault возвращает значение переменной окружения или fallback, если оно пустое.
func getenvOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// heartbeat логирует, что бот работает, каждые 60 секунд (для мониторинга).
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
