package rocketbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var (
	// ErrNotConnected is returned when an operation requires an active connection.
	ErrNotConnected = errors.New("rocketbot: not connected")
)

// Config contains parameters needed to connect to Rocket.Chat over WebSocket (DDP).
type Config struct {
	// ServerURL is your Rocket.Chat base URL, for example: https://chat.example.com
	ServerURL string

	// UserID is the user id of bot account (X-User-Id / userId).
	UserID string

	// AuthToken is auth token for bot account (X-Auth-Token / resume token).
	AuthToken string

	// Optional custom HTTP client used by websocket dialer.
	HTTPClient *http.Client
}

// Message represents a chat message event from Rocket.Chat stream-room-messages.
type Message struct {
	ID       string
	RoomID   string
	Text     string
	SenderID string
	Raw      json.RawMessage
}

// Client is a DDP client for building Rocket.Chat bots.
type Client struct {
	cfg  Config
	conn *websocket.Conn

	writeMu sync.Mutex
	stateMu sync.RWMutex

	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]chan ddpFrame

	onMessage func(context.Context, Message)
}

// NewClient creates a new bot client.
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:     cfg,
		pending: make(map[string]chan ddpFrame),
	}
}

// OnMessage sets callback invoked on each room message event.
func (c *Client) OnMessage(handler func(context.Context, Message)) {
	c.onMessage = handler
}

// Connect opens websocket, performs DDP connect and bot login (resume token auth).
func (c *Client) Connect(ctx context.Context) error {
	if strings.TrimSpace(c.cfg.ServerURL) == "" {
		return errors.New("rocketbot: ServerURL is required")
	}
	if strings.TrimSpace(c.cfg.UserID) == "" {
		return errors.New("rocketbot: UserID is required")
	}
	if strings.TrimSpace(c.cfg.AuthToken) == "" {
		return errors.New("rocketbot: AuthToken is required")
	}

	wsURL, err := rocketWebSocketURL(c.cfg.ServerURL)
	if err != nil {
		return err
	}

	opts := &websocket.DialOptions{
		HTTPClient: c.cfg.HTTPClient,
	}
	conn, _, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		return fmt.Errorf("rocketbot: dial websocket: %w", err)
	}

	c.stateMu.Lock()
	c.conn = conn
	c.stateMu.Unlock()

	go c.readLoop()

	// Step 1: DDP connect handshake.
	if _, err := c.sendAndWait(ctx, ddpFrame{
		Msg:     "connect",
		Version: "1",
		Support: []string{"1", "pre2", "pre1"},
	}, nil); err != nil {
		_ = c.Close(websocket.StatusInternalError, "connect failed")
		return fmt.Errorf("rocketbot: ddp connect: %w", err)
	}

	// Step 2: Authenticate using resume token.
	loginResult := struct {
		ID      string `json:"id"`
		Token   string `json:"token"`
		TokenAt int64  `json:"tokenExpires"`
	}{}

	_, err = c.sendAndWait(ctx, ddpFrame{
		Msg:    "method",
		Method: "login",
		Params: []any{
			map[string]string{
				"resume": c.cfg.AuthToken,
			},
		},
	}, &loginResult)
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "login failed")
		return fmt.Errorf("rocketbot: login failed: %w", err)
	}

	if loginResult.ID != "" && loginResult.ID != c.cfg.UserID {
		_ = c.Close(websocket.StatusPolicyViolation, "wrong user")
		return fmt.Errorf("rocketbot: login user mismatch: expected %q, got %q", c.cfg.UserID, loginResult.ID)
	}

	return nil
}

// Close closes websocket connection.
func (c *Client) Close(code websocket.StatusCode, reason string) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close(code, reason)
	c.conn = nil
	return err
}

// SubscribeRoomMessages subscribes to message stream for a specific room/channel.
func (c *Client) SubscribeRoomMessages(ctx context.Context, roomID string) error {
	if strings.TrimSpace(roomID) == "" {
		return errors.New("rocketbot: roomID is required")
	}

	_, err := c.sendAndWait(ctx, ddpFrame{
		Msg:  "sub",
		Name: "stream-room-messages",
		Params: []any{
			roomID,
			false,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("rocketbot: subscribe room messages: %w", err)
	}
	return nil
}

// SendMessage sends a plain text message to room/channel.
func (c *Client) SendMessage(ctx context.Context, roomID, text string) error {
	if strings.TrimSpace(roomID) == "" {
		return errors.New("rocketbot: roomID is required")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("rocketbot: text is required")
	}

	_, err := c.sendAndWait(ctx, ddpFrame{
		Msg:    "method",
		Method: "sendMessage",
		Params: []any{
			map[string]string{
				"rid": roomID,
				"msg": text,
			},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("rocketbot: send message: %w", err)
	}
	return nil
}

func (c *Client) sendAndWait(ctx context.Context, frame ddpFrame, resultTarget any) (ddpFrame, error) {
	conn := c.getConn()
	if conn == nil {
		return ddpFrame{}, ErrNotConnected
	}

	id := c.nextFrameID()
	frame.ID = id

	waitCh := make(chan ddpFrame, 1)
	c.pendingMu.Lock()
	c.pending[id] = waitCh
	c.pendingMu.Unlock()

	if err := c.writeJSON(ctx, frame); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return ddpFrame{}, err
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return ddpFrame{}, ctx.Err()
	case reply := <-waitCh:
		if reply.Error != nil {
			return ddpFrame{}, fmt.Errorf("code=%d reason=%s message=%s", reply.Error.Error, reply.Error.Reason, reply.Error.Message)
		}
		if resultTarget != nil && len(reply.Result) > 0 && string(reply.Result) != "null" {
			if err := json.Unmarshal(reply.Result, resultTarget); err != nil {
				return ddpFrame{}, fmt.Errorf("unmarshal method result: %w", err)
			}
		}
		return reply, nil
	}
}

func (c *Client) readLoop() {
	for {
		conn := c.getConn()
		if conn == nil {
			return
		}

		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}

		var frame ddpFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}

		switch frame.Msg {
		case "ping":
			_ = c.writeJSON(context.Background(), ddpFrame{Msg: "pong"})
		case "connected":
			c.resolvePending(frame.ID, frame)
		case "result", "ready", "nosub":
			c.resolvePending(frame.ID, frame)
		case "changed":
			c.handleChanged(frame)
		}
	}
}

func (c *Client) handleChanged(frame ddpFrame) {
	if frame.Collection != "stream-room-messages" || c.onMessage == nil {
		return
	}

	var payload []json.RawMessage
	if err := json.Unmarshal(frame.Fields.Args, &payload); err != nil {
		return
	}
	if len(payload) < 1 {
		return
	}

	var event struct {
		ID  string `json:"_id"`
		RID string `json:"rid"`
		Msg string `json:"msg"`
		U   struct {
			ID string `json:"_id"`
		} `json:"u"`
	}
	if err := json.Unmarshal(payload[0], &event); err != nil {
		return
	}

	c.onMessage(context.Background(), Message{
		ID:       event.ID,
		RoomID:   event.RID,
		Text:     event.Msg,
		SenderID: event.U.ID,
		Raw:      payload[0],
	})
}

func (c *Client) resolvePending(id string, frame ddpFrame) {
	if id == "" {
		return
	}
	c.pendingMu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	if ok {
		ch <- frame
	}
}

func (c *Client) getConn() *websocket.Conn {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.conn
}

func (c *Client) nextFrameID() string {
	return fmt.Sprintf("%d", c.nextID.Add(1))
}

func (c *Client) writeJSON(ctx context.Context, frame ddpFrame) error {
	conn := c.getConn()
	if conn == nil {
		return ErrNotConnected
	}

	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return conn.Write(writeCtx, websocket.MessageText, payload)
}

func rocketWebSocketURL(serverURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return "", fmt.Errorf("rocketbot: parse ServerURL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("rocketbot: ServerURL must start with http:// or https://")
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}

	u.Path = strings.TrimRight(u.Path, "/") + "/websocket"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

type ddpFrame struct {
	Msg        string          `json:"msg"`
	ID         string          `json:"id,omitempty"`
	Version    string          `json:"version,omitempty"`
	Support    []string        `json:"support,omitempty"`
	Method     string          `json:"method,omitempty"`
	Params     []any           `json:"params,omitempty"`
	Name       string          `json:"name,omitempty"`
	Collection string          `json:"collection,omitempty"`
	Fields     ddpFields       `json:"fields,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *ddpError       `json:"error,omitempty"`
}

type ddpFields struct {
	Args json.RawMessage `json:"args,omitempty"`
}

type ddpError struct {
	Error   int    `json:"error"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}
