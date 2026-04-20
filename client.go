package rocketbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

// AutoSubscribeOptions controls background room auto-subscription.
type AutoSubscribeOptions struct {
	// Optional static room IDs to subscribe once on startup.
	StaticRoomIDs []string
	// Poll interval for discovering new room subscriptions.
	Interval time.Duration
	// Optional callback called for each successfully subscribed room id.
	OnSubscribed func(roomID string)
	// Optional callback for auto-subscribe/discovery errors.
	OnError func(error)
	// Optional callback after each discovery pass.
	OnDiscovered func(roomCount int)
}

// Message represents a chat message event from Rocket.Chat stream-room-messages.
type Message struct {
	ID       string
	RoomID   string
	Text     string
	SenderID string
	Raw      json.RawMessage
}

// AttachmentField is a table-like field in message attachment.
type AttachmentField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short,omitempty"`
}

// Attachment is a rich Rocket.Chat message attachment.
type Attachment struct {
	Title  string            `json:"title,omitempty"`
	Text   string            `json:"text,omitempty"`
	Color  string            `json:"color,omitempty"`
	Fields []AttachmentField `json:"fields,omitempty"`
	Ts     time.Time         `json:"ts,omitempty"`
}

// UploadResult contains parsed response of room file upload.
type UploadResult struct {
	MessageID string
	FileURL   string
	Raw       string
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
		TokenAt any    `json:"tokenExpires"`
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

// SendMessageWithAttachment sends a message with one attachment to room/channel.
func (c *Client) SendMessageWithAttachment(ctx context.Context, roomID string, attachment Attachment) error {
	return c.SendMessageWithAttachments(ctx, roomID, "", []Attachment{attachment})
}

// SendMessageWithAttachments sends a message with attachments to room/channel.
func (c *Client) SendMessageWithAttachments(ctx context.Context, roomID, text string, attachments []Attachment) error {
	if strings.TrimSpace(roomID) == "" {
		return errors.New("rocketbot: roomID is required")
	}
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		return errors.New("rocketbot: text or attachments is required")
	}

	payload := map[string]any{
		"roomId": roomID,
		"text":   text,
	}
	if strings.TrimSpace(text) == "" {
		payload["text"] = " "
	}
	if len(attachments) > 0 {
		payload["attachments"] = toAPIAttachments(attachments)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("rocketbot: marshal chat.postMessage payload: %w", err)
	}

	req, err := c.newAuthorizedRequest(ctx, http.MethodPost, "/api/v1/chat.postMessage")
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("rocketbot: request chat.postMessage: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respText := strings.TrimSpace(string(respBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rocketbot: chat.postMessage returned %s: %s", resp.Status, respText)
	}

	var apiResp struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("rocketbot: decode chat.postMessage response: %w; body=%q", err, respText)
	}
	if !apiResp.Success {
		return fmt.Errorf("rocketbot: chat.postMessage success=false: %s", respText)
	}
	return nil
}

// UploadRoomFile uploads a file to a room using REST API.
// It tries /api/v1/rooms.upload first and falls back to /api/v1/rooms.media
// for servers where the old route is unavailable.
func (c *Client) UploadRoomFile(ctx context.Context, roomID, filePath, msg string) error {
	_, err := c.UploadRoomFileDetailed(ctx, roomID, filePath, msg)
	return err
}

// UploadRoomFileDetailed uploads file and returns parsed upload response.
func (c *Client) UploadRoomFileDetailed(ctx context.Context, roomID, filePath, msg string) (UploadResult, error) {
	if strings.TrimSpace(roomID) == "" {
		return UploadResult{}, errors.New("rocketbot: roomID is required")
	}
	if strings.TrimSpace(filePath) == "" {
		return UploadResult{}, errors.New("rocketbot: filePath is required")
	}

	uploadOnce := func(endpoint string) (statusCode int, responseText string, err error) {
		file, err := os.Open(filePath)
		if err != nil {
			return 0, "", fmt.Errorf("rocketbot: open file %q: %w", filePath, err)
		}
		defer file.Close()

		var body bytes.Buffer
		writer := multipart.NewWriter(&body)

		filePart, err := writer.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			return 0, "", fmt.Errorf("rocketbot: create multipart file part: %w", err)
		}
		if _, err := io.Copy(filePart, file); err != nil {
			return 0, "", fmt.Errorf("rocketbot: write file to multipart body: %w", err)
		}
		if strings.TrimSpace(msg) != "" {
			if err := writer.WriteField("msg", msg); err != nil {
				return 0, "", fmt.Errorf("rocketbot: write multipart msg field: %w", err)
			}
		}
		if err := writer.Close(); err != nil {
			return 0, "", fmt.Errorf("rocketbot: close multipart writer: %w", err)
		}

		req, err := c.newAuthorizedRequest(ctx, http.MethodPost, endpoint+roomID)
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
		req.ContentLength = int64(body.Len())

		resp, err := c.httpClient().Do(req)
		if err != nil {
			return 0, "", fmt.Errorf("rocketbot: upload request failed: %w", err)
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(respBody)), nil
	}

	statusCode, responseText, err := uploadOnce("/api/v1/rooms.upload/")
	if err != nil {
		return UploadResult{}, err
	}
	if statusCode == http.StatusNotFound {
		statusCode, responseText, err = uploadOnce("/api/v1/rooms.media/")
		if err != nil {
			return UploadResult{}, err
		}
	}
	if statusCode != http.StatusOK {
		return UploadResult{}, fmt.Errorf("rocketbot: upload failed with status %d: %s", statusCode, responseText)
	}
	var apiResp struct {
		Success bool `json:"success"`
		Message struct {
			ID string `json:"_id"`
		} `json:"message"`
		File struct {
			URL string `json:"url"`
		} `json:"file"`
	}
	if err := json.Unmarshal([]byte(responseText), &apiResp); err != nil {
		return UploadResult{}, fmt.Errorf("rocketbot: upload returned non-json response: %q", responseText)
	}
	if !apiResp.Success {
		return UploadResult{}, fmt.Errorf("rocketbot: upload returned success=false: %s", responseText)
	}
	fileURL := strings.TrimSpace(apiResp.File.URL)
	if fileURL != "" && strings.HasPrefix(fileURL, "/") {
		fileURL = strings.TrimRight(c.cfg.ServerURL, "/") + fileURL
	}
	messageID := strings.TrimSpace(apiResp.Message.ID)
	// Some Rocket.Chat versions upload file successfully but do not create message.
	// In that case publish a message with attachment image_url explicitly.
	if messageID == "" && fileURL != "" {
		id, err := c.sendImageURLMessage(ctx, roomID, fileURL, msg)
		if err != nil {
			return UploadResult{}, err
		}
		messageID = id
	}
	return UploadResult{
		MessageID: messageID,
		FileURL:   fileURL,
		Raw:       responseText,
	}, nil
}

func (c *Client) sendImageURLMessage(ctx context.Context, roomID, imageURL, text string) (string, error) {
	payload := map[string]any{
		"roomId": roomID,
		"text":   text,
		"attachments": []map[string]any{
			{
				"image_url": imageURL,
			},
		},
	}
	if strings.TrimSpace(text) == "" {
		payload["text"] = " "
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("rocketbot: marshal image_url post payload: %w", err)
	}
	req, err := c.newAuthorizedRequest(ctx, http.MethodPost, "/api/v1/chat.postMessage")
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("rocketbot: request chat.postMessage for image: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	respText := strings.TrimSpace(string(respBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("rocketbot: chat.postMessage(image_url) returned %s: %s", resp.Status, respText)
	}

	var apiResp struct {
		Success bool `json:"success"`
		Message struct {
			ID string `json:"_id"`
		} `json:"message"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return "", fmt.Errorf("rocketbot: decode chat.postMessage(image_url) response: %w; body=%q", err, respText)
	}
	if !apiResp.Success {
		return "", fmt.Errorf("rocketbot: chat.postMessage(image_url) success=false: %s", respText)
	}
	return strings.TrimSpace(apiResp.Message.ID), nil
}

func toAPIAttachments(attachments []Attachment) []map[string]any {
	out := make([]map[string]any, 0, len(attachments))
	for _, a := range attachments {
		item := map[string]any{}
		if strings.TrimSpace(a.Title) != "" {
			item["title"] = a.Title
		}
		if strings.TrimSpace(a.Text) != "" {
			item["text"] = a.Text
		}
		if strings.TrimSpace(a.Color) != "" {
			item["color"] = a.Color
		}
		if len(a.Fields) > 0 {
			fields := make([]map[string]any, 0, len(a.Fields))
			for _, f := range a.Fields {
				field := map[string]any{
					"title": f.Title,
					"value": f.Value,
				}
				if f.Short {
					field["short"] = true
				}
				fields = append(fields, field)
			}
			item["fields"] = fields
		}
		if !a.Ts.IsZero() {
			item["ts"] = a.Ts.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	return out
}

// ValidateAuth verifies X-User-Id and X-Auth-Token against /api/v1/me.
func (c *Client) ValidateAuth(ctx context.Context) error {
	req, err := c.newAuthorizedRequest(ctx, http.MethodGet, "/api/v1/me")
	if err != nil {
		return err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("rocketbot: request /api/v1/me: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyText := strings.TrimSpace(string(body))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rocketbot: /api/v1/me returned %s: %s", resp.Status, bodyText)
	}
	if bodyText == "" {
		return errors.New("rocketbot: /api/v1/me returned empty response body")
	}

	var payload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("rocketbot: decode /api/v1/me: %w; body=%q", err, bodyText)
	}
	if !payload.Success {
		return fmt.Errorf("rocketbot: /api/v1/me returned success=false: %s", bodyText)
	}
	return nil
}

// ListSubscribedRoomIDs returns current room ids from /api/v1/subscriptions.get.
func (c *Client) ListSubscribedRoomIDs(ctx context.Context) ([]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := c.newAuthorizedRequest(reqCtx, http.MethodGet, "/api/v1/subscriptions.get")
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("rocketbot: request subscriptions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rocketbot: subscriptions request failed with status %s", resp.Status)
	}

	var payload struct {
		Update []struct {
			RID string `json:"rid"`
		} `json:"update"`
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("rocketbot: decode subscriptions response: %w", err)
	}
	if !payload.Success {
		return nil, errors.New("rocketbot: subscriptions response returned success=false")
	}

	roomIDs := make([]string, 0, len(payload.Update))
	seen := make(map[string]struct{}, len(payload.Update))
	for _, item := range payload.Update {
		rid := strings.TrimSpace(item.RID)
		if rid == "" {
			continue
		}
		if _, ok := seen[rid]; ok {
			continue
		}
		seen[rid] = struct{}{}
		roomIDs = append(roomIDs, rid)
	}
	return roomIDs, nil
}

// StartAutoSubscribeRooms starts background room subscription management.
func (c *Client) StartAutoSubscribeRooms(ctx context.Context, opts AutoSubscribeOptions) {
	interval := opts.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	subscribed := make(map[string]struct{})
	var subscribedMu sync.Mutex

	subscribeRooms := func(ids []string) {
		for _, roomID := range ids {
			roomID = strings.TrimSpace(roomID)
			if roomID == "" {
				continue
			}

			subscribedMu.Lock()
			_, already := subscribed[roomID]
			subscribedMu.Unlock()
			if already {
				continue
			}

			if err := c.SubscribeRoomMessages(ctx, roomID); err != nil {
				if opts.OnError != nil {
					opts.OnError(fmt.Errorf("subscribe failed for room %s: %w", roomID, err))
				}
				continue
			}

			subscribedMu.Lock()
			subscribed[roomID] = struct{}{}
			subscribedMu.Unlock()
			if opts.OnSubscribed != nil {
				opts.OnSubscribed(roomID)
			}
		}
	}

	go func() {
		if len(opts.StaticRoomIDs) > 0 {
			subscribeRooms(opts.StaticRoomIDs)
		} else {
			roomIDs, err := c.ListSubscribedRoomIDs(ctx)
			if err != nil {
				if opts.OnError != nil {
					opts.OnError(fmt.Errorf("auto-discover failed: %w", err))
				}
			} else {
				subscribeRooms(roomIDs)
				if opts.OnDiscovered != nil {
					opts.OnDiscovered(len(roomIDs))
				}
			}
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				roomIDs, err := c.ListSubscribedRoomIDs(ctx)
				if err != nil {
					if opts.OnError != nil {
						opts.OnError(fmt.Errorf("room auto-discovery failed: %w", err))
					}
					continue
				}
				subscribeRooms(roomIDs)
				if opts.OnDiscovered != nil {
					opts.OnDiscovered(len(roomIDs))
				}
			}
		}
	}()
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
			// DDP "connected" frame usually has no id, so resolve the first pending request.
			c.resolveFirstPending(frame)
		case "ready":
			c.resolvePendingBySubs(frame)
		case "result", "nosub":
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

	type roomEvent struct {
		ID  string `json:"_id"`
		RID string `json:"rid"`
		Msg string `json:"msg"`
		U   struct {
			ID string `json:"_id"`
		} `json:"u"`
	}

	parseEvent := func(raw json.RawMessage) (roomEvent, bool) {
		var event roomEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return roomEvent{}, false
		}
		if strings.TrimSpace(event.RID) == "" || strings.TrimSpace(event.U.ID) == "" {
			return roomEvent{}, false
		}
		return event, true
	}

	var event roomEvent
	found := false
	// Rocket.Chat can send different arg layouts for stream-room-messages;
	// scan args and pick the first object that looks like a message event.
	for _, candidate := range payload {
		if ev, ok := parseEvent(candidate); ok {
			event = ev
			found = true
			break
		}
	}
	if !found {
		return
	}

	msg := Message{
		ID:       event.ID,
		RoomID:   event.RID,
		Text:     event.Msg,
		SenderID: event.U.ID,
		Raw:      payload[0],
	}
	// Run user callback asynchronously so readLoop can keep processing
	// DDP frames (including sendMessage "result" replies).
	go c.onMessage(context.Background(), msg)
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

func (c *Client) resolveFirstPending(frame ddpFrame) {
	c.pendingMu.Lock()
	var (
		firstID string
		ch      chan ddpFrame
	)
	for id, pendingCh := range c.pending {
		firstID = id
		ch = pendingCh
		break
	}
	if firstID != "" {
		delete(c.pending, firstID)
	}
	c.pendingMu.Unlock()
	if ch != nil {
		ch <- frame
	}
}

func (c *Client) resolvePendingBySubs(frame ddpFrame) {
	c.pendingMu.Lock()
	channels := make([]chan ddpFrame, 0, len(frame.Subs))
	for _, subID := range frame.Subs {
		if ch, ok := c.pending[subID]; ok {
			delete(c.pending, subID)
			channels = append(channels, ch)
		}
	}
	c.pendingMu.Unlock()

	for _, ch := range channels {
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

func (c *Client) httpClient() *http.Client {
	if c.cfg.HTTPClient != nil {
		return c.cfg.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) newAuthorizedRequest(ctx context.Context, method, endpoint string) (*http.Request, error) {
	baseURL := strings.TrimRight(c.cfg.ServerURL, "/")
	if baseURL == "" {
		return nil, errors.New("rocketbot: ServerURL is required")
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("rocketbot: build request %s: %w", endpoint, err)
	}
	req.Header.Set("X-User-Id", c.cfg.UserID)
	req.Header.Set("X-Auth-Token", c.cfg.AuthToken)
	req.Header.Set("Accept", "application/json")
	return req, nil
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
	Subs       []string        `json:"subs,omitempty"`
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
