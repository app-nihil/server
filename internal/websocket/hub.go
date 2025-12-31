package websocket

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"nihil/internal/firebase"
	redisdb "nihil/internal/redis"
)

var (
	ErrClientBufferFull = errors.New("client send buffer full")
	ErrNotAuthed        = errors.New("client not authenticated")
	ErrChatNotFound     = errors.New("chat not found")
)

type Hub struct {
	clients            map[string]*Client
	connections        map[*Client]bool
	register           chan *Client
	unregister         chan *Client
	redis              *redisdb.Client
	rateLimitPerMinute int
	mu                 sync.RWMutex
}

func NewHub(redis *redisdb.Client, rateLimitPerMinute int) *Hub {
	return &Hub{
		clients:            make(map[string]*Client),
		connections:        make(map[*Client]bool),
		register:           make(chan *Client),
		unregister:         make(chan *Client),
		redis:              redis,
		rateLimitPerMinute: rateLimitPerMinute,
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.connections[client] = true
			h.mu.Unlock()

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.connections[client]; ok {
				delete(h.connections, client)
				if client.deviceUUID != "" {
					delete(h.clients, client.deviceUUID)
				}
				client.Close()
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) Register(client *Client) {
	h.register <- client
}

func (h *Hub) HandleMessage(client *Client, msg *WSMessage) {
	ctx := context.Background()

	switch msg.Type {
	case TypeAuth:
		h.handleAuth(ctx, client, msg)
	case TypeMessageSend:
		h.handleMessageSend(ctx, client, msg)
	case TypeMessageRead:
		h.handleMessageRead(ctx, client, msg)
	case TypeTypingStart, TypeTypingStop:
		h.handleTyping(ctx, client, msg)
	case "ping":
		// Heartbeat - just ignore, connection is alive
		return
	default:
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "unknown_type",
				Message: fmt.Sprintf("Unknown message type: %s", msg.Type),
			},
		})
	}
}

func (h *Hub) handleAuth(ctx context.Context, client *Client, msg *WSMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload AuthPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		client.SendMessage(&WSMessage{
			Type:    TypeAuthFailed,
			Payload: AuthFailedPayload{Reason: "invalid_payload"},
		})
		return
	}

	banned, reason, _ := h.redis.IsBanned(ctx, payload.DeviceUUID)
	if banned {
		client.SendMessage(&WSMessage{
			Type:    TypeBanned,
			Payload: BannedPayload{Reason: reason},
		})
		return
	}

	now := time.Now().Unix()
	if abs(now-payload.Timestamp) > 300 {
		client.SendMessage(&WSMessage{
			Type:    TypeAuthFailed,
			Payload: AuthFailedPayload{Reason: "timestamp_expired"},
		})
		return
	}

	publicKey, err := h.redis.GetDevicePublicKey(ctx, payload.DeviceUUID)
	if err != nil {
		client.SendMessage(&WSMessage{
			Type:    TypeAuthFailed,
			Payload: AuthFailedPayload{Reason: "device_not_found"},
		})
		return
	}

	expectedSig := computeSignature(publicKey, payload.DeviceUUID, payload.Timestamp)
	if payload.Signature != expectedSig {
		client.SendMessage(&WSMessage{
			Type:    TypeAuthFailed,
			Payload: AuthFailedPayload{Reason: "invalid_signature"},
		})
		return
	}

	sub, err := h.redis.GetSubscription(ctx, payload.DeviceUUID)
	if err != nil || sub.Status != "active" || time.Now().After(sub.ExpiresAt) {
		client.SendMessage(&WSMessage{
			Type:    TypeSubExpired,
			Payload: SubExpiredPayload{RenewURL: "https://nihil.app"},
		})
		return
	}

	client.SetDeviceUUID(payload.DeviceUUID)

	h.mu.Lock()
	h.clients[payload.DeviceUUID] = client
	h.mu.Unlock()

	chatUUIDs, _ := h.redis.GetUserChats(ctx, payload.DeviceUUID)
	chats := make([]ChatInfo, 0, len(chatUUIDs))

	log.Printf("User %s has %d chats", payload.DeviceUUID, len(chatUUIDs))

	for _, chatUUID := range chatUUIDs {
		chat, err := h.redis.GetChat(ctx, chatUUID)
		if err != nil {
			log.Printf("Error getting chat %s: %v", chatUUID, err)
			continue
		}

		otherDevice := ""
		if chat.ParticipantA == payload.DeviceUUID {
			otherDevice = chat.ParticipantB
		} else {
			otherDevice = chat.ParticipantA
		}

		chats = append(chats, ChatInfo{
			ChatUUID:    chat.ChatUUID,
			TTLSeconds:  chat.TTLSeconds,
			Status:      chat.Status,
			CreatedAt:   chat.CreatedAt,
			OtherDevice: otherDevice,
		})
	}

	// Deliver queued messages with SenderUUID
	for _, chatUUID := range chatUUIDs {
		messages, err := h.redis.GetQueuedMessages(ctx, chatUUID)
		log.Printf("Queued messages for chat %s: count=%d, err=%v", chatUUID, len(messages), err)
		
		for msgID, queuedMsg := range messages {
			log.Printf("Delivering queued message %s from %s to %s", msgID, queuedMsg.SenderUUID, payload.DeviceUUID)
			
			// Only deliver to recipient, not sender
			if queuedMsg.SenderUUID == payload.DeviceUUID {
				log.Printf("Skipping message %s - sender is same as recipient", msgID)
				continue
			}
			
			client.SendMessage(&WSMessage{
				Type: TypeMessageReceived,
				Payload: MessageReceivedPayload{
					ChatUUID:         chatUUID,
					MessageID:        msgID,
					SenderUUID:       queuedMsg.SenderUUID,
					EncryptedContent: base64.StdEncoding.EncodeToString(queuedMsg.EncryptedContent),
					Timestamp:        time.Now().Unix(),
				},
			})
			log.Printf("Message %s sent to client", msgID)
		}
	}

	client.SendMessage(&WSMessage{
		Type: TypeAuthSuccess,
		Payload: AuthSuccessPayload{
			Chats: chats,
			Subscription: SubscriptionInfo{
				Plan:      sub.Plan,
				ExpiresAt: sub.ExpiresAt,
			},
		},
	})

	log.Printf("Client authenticated: %s", payload.DeviceUUID)
}

func (h *Hub) handleMessageSend(ctx context.Context, client *Client, msg *WSMessage) {
	if !client.IsAuthed() {
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "not_authenticated",
				Message: "Must authenticate first",
			},
		})
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MessageSendPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return
	}

	deviceUUID := client.GetDeviceUUID()

	count, allowed, _ := h.redis.CheckRateLimit(ctx, deviceUUID, h.rateLimitPerMinute)
	if !allowed {
		action, _ := h.redis.HandleAbuse(ctx, deviceUUID, "rate_limit_exceeded")
		if action == "ban" {
			client.SendMessage(&WSMessage{
				Type:    TypeBanned,
				Payload: BannedPayload{Reason: "rate_limit_abuse"},
			})
			h.unregister <- client
			return
		}
		client.SendMessage(&WSMessage{
			Type: TypeRateLimitWarning,
			Payload: RateLimitWarningPayload{
				Current: count,
				Limit:   h.rateLimitPerMinute,
			},
		})
		return
	}

	chat, err := h.redis.GetChat(ctx, payload.ChatUUID)
	if err != nil {
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "chat_not_found",
				Message: "Chat not found",
			},
		})
		return
	}

	if chat.ParticipantA != deviceUUID && chat.ParticipantB != deviceUUID {
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "not_participant",
				Message: "Not a participant of this chat",
			},
		})
		return
	}

	recipientUUID := chat.ParticipantA
	if chat.ParticipantA == deviceUUID {
		recipientUUID = chat.ParticipantB
	}

	content, err := base64.StdEncoding.DecodeString(payload.EncryptedContent)
	if err != nil || len(content) > 10240 {
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "message_too_large",
				Message: "Message exceeds 10KB limit",
			},
		})
		return
	}

	msgHash := sha256Hash(string(content))
	if err := h.redis.RecordMessage(ctx, deviceUUID, msgHash); err != nil {
		action, _ := h.redis.HandleAbuse(ctx, deviceUUID, err.Error())
		if action == "ban" {
			client.SendMessage(&WSMessage{
				Type:    TypeBanned,
				Payload: BannedPayload{Reason: "abuse"},
			})
			h.unregister <- client
			return
		}
	}

	h.mu.RLock()
	recipient, online := h.clients[recipientUUID]
	h.mu.RUnlock()

	outMsg := &WSMessage{
		Type: TypeMessageReceived,
		Payload: MessageReceivedPayload{
			ChatUUID:         payload.ChatUUID,
			MessageID:        payload.MessageID,
			SenderUUID:       deviceUUID,
			EncryptedContent: payload.EncryptedContent,
			Timestamp:        time.Now().Unix(),
		},
	}

	if online {
		recipient.SendMessage(outMsg)
	} else {
		// Queue message for later delivery - include senderUUID
		h.redis.QueueMessage(ctx, payload.ChatUUID, payload.MessageID, deviceUUID, content)

		// Send push notification to wake up the app
		h.sendPushNotification(ctx, recipientUUID, payload.ChatUUID)
	}
}

// sendPushNotification sends a silent push to wake up the recipient's app
func (h *Hub) sendPushNotification(ctx context.Context, recipientUUID, chatUUID string) {
	if !firebase.IsInitialized() {
		log.Printf("Firebase not initialized, skipping push for %s", recipientUUID)
		return
	}

	fcmToken, err := h.redis.GetFCMToken(ctx, recipientUUID)
	if err != nil {
		log.Printf("No FCM token for device %s: %v", recipientUUID, err)
		return
	}

	// Send data-only message (silent push) - no notification shown
	// App wakes up and connects to WebSocket to get messages
	data := map[string]string{
		"type":      "new_message",
		"chat_uuid": chatUUID,
	}

	if err := firebase.SendPush(ctx, fcmToken, data); err != nil {
		log.Printf("Failed to send push to %s: %v", recipientUUID, err)
		return
	}

	log.Printf("Push sent to %s for chat %s", recipientUUID, chatUUID)
}

func (h *Hub) handleMessageRead(ctx context.Context, client *Client, msg *WSMessage) {
	if !client.IsAuthed() {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload MessageReadPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return
	}

	h.redis.DeleteQueuedMessage(ctx, payload.ChatUUID, payload.MessageID)

	chat, err := h.redis.GetChat(ctx, payload.ChatUUID)
	if err != nil {
		return
	}

	deviceUUID := client.GetDeviceUUID()
	otherUUID := chat.ParticipantA
	if chat.ParticipantA == deviceUUID {
		otherUUID = chat.ParticipantB
	}

	h.mu.RLock()
	other, online := h.clients[otherUUID]
	h.mu.RUnlock()

	if online {
		other.SendMessage(&WSMessage{
			Type: TypeMessageReadAck,
			Payload: MessageReadAckPayload{
				ChatUUID:  payload.ChatUUID,
				MessageID: payload.MessageID,
			},
		})
	}
}

func (h *Hub) handleTyping(ctx context.Context, client *Client, msg *WSMessage) {
	if !client.IsAuthed() {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload TypingPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return
	}

	chat, err := h.redis.GetChat(ctx, payload.ChatUUID)
	if err != nil {
		return
	}

	deviceUUID := client.GetDeviceUUID()
	otherUUID := chat.ParticipantA
	if chat.ParticipantA == deviceUUID {
		otherUUID = chat.ParticipantB
	}

	h.mu.RLock()
	other, online := h.clients[otherUUID]
	h.mu.RUnlock()

	if online {
		other.SendMessage(&WSMessage{
			Type: TypeTypingIndicator,
			Payload: TypingPayload{
				ChatUUID: payload.ChatUUID,
			},
		})
	}
}

func (h *Hub) GetClient(deviceUUID string) (*Client, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	client, ok := h.clients[deviceUUID]
	return client, ok
}

func (h *Hub) BroadcastToChat(ctx context.Context, chatUUID string, msg *WSMessage) error {
	chat, err := h.redis.GetChat(ctx, chatUUID)
	if err != nil {
		return err
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	if clientA, ok := h.clients[chat.ParticipantA]; ok {
		clientA.SendMessage(msg)
	}
	if clientB, ok := h.clients[chat.ParticipantB]; ok {
		clientB.SendMessage(msg)
	}

	return nil
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func computeSignature(key, deviceUUID string, timestamp int64) string {
	data := fmt.Sprintf("%s:%d", deviceUUID, timestamp)
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hash(data string) string {
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}
