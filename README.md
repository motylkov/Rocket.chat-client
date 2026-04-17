# rocket_bot

Go-библиотека для создания ботов в Rocket.Chat через WebSocket (DDP)`.

- подключение к Rocket.Chat по WebSocket (`/websocket`);
- авторизация бота через `resume` token (`AuthToken`);
- подписка на сообщения комнаты (`stream-room-messages`);
- отправка сообщений в комнату (`sendMessage`);
- обработка входящих сообщений через callback.


Пример создания клиента:

```go
bot := rocketbot.NewClient(rocketbot.Config{
	ServerURL: "https://chat.example.com",
	UserID:    "your-bot-user-id",
	AuthToken: "your-bot-auth-token",
})
```

