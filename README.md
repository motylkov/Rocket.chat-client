# Rocket.chat-client

Go-библиотека для создания ботов в Rocket.Chat через WebSocket (DDP)`.

- подключение к Rocket.Chat по WebSocket (`/websocket`);
- авторизация бота через `resume` token (`AuthToken`);
- подписка на сообщения комнаты (`stream-room-messages`);
- отправка сообщений в комнату (`sendMessage`);
- обработка входящих сообщений через callback.

Для работы бота нужны:

- `ServerURL` — базовый адрес Rocket.Chat, например `https://chat.example.com`;
- `UserID` — ID пользователя бота;
- `AuthToken` — токен авторизации пользователя бота (можно использовать Personal Access Token);
- `roomID` — ID комнаты/канала, где бот слушает и/или отправляет сообщения.


Пример создания клиента:

```go
import 	rocketbot "github.com/motylkov/Rocket.chat-client"

bot := rocketbot.NewClient(rocketbot.Config{
	ServerURL: "https://chat.example.com",
	UserID:    "your-bot-user-id",
	AuthToken: "your-bot-auth-token",
})
```
