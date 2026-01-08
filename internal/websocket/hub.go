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

// chatParticipantKey creates a unique key for chat+participant mapping
func chatParticipantKey(chatUUID, participantID string) string {
	return chatUUID + ":" + participantID
}

type Hub struct {
	clients            map[string]*Client          // deviceUUID -> Client
	connections        map[*Client]bool            // all connections
	chatParticipants   map[string]string           // chatUUID:participantID -> deviceUUID
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
		chatParticipants:   make(map[string]string),
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
			fmt.Printf("[DEBUG] Client connected (total connections: %d)\n", len(h.connections))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.connections[client]; ok {
				delete(h.connections, client)
				if client.deviceUUID != "" {
					fmt.Printf("[DEBUG] Client disconnected: %s\n", client.deviceUUID)
					delete(h.clients, client.deviceUUID)
					// Clean up chat participant mappings for this device
					for key, deviceUUID := range h.chatParticipants {
						if deviceUUID == client.deviceUUID {
							fmt.Printf("[DEBUG] Removing mapping: %s\n", key)
							delete(h.chatParticipants, key)
						}
					}
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

// DisconnectDevice forcefully disconnects a device and clears all in-memory state
// Called when device is purged via HTTP API
func (h *Hub) DisconnectDevice(deviceUUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	client, exists := h.clients[deviceUUID]
	if !exists {
		fmt.Printf("[DEBUG] DisconnectDevice: device %s not connected\n", deviceUUID)
		return
	}

	fmt.Printf("[DEBUG] DisconnectDevice: forcefully disconnecting %s\n", deviceUUID)

	// Remove from clients map
	delete(h.clients, deviceUUID)

	// Remove from connections
	delete(h.connections, client)

	// Clean up all chat participant mappings for this device
	for key, devUUID := range h.chatParticipants {
		if devUUID == deviceUUID {
			fmt.Printf("[DEBUG] DisconnectDevice: removing mapping %s\n", key)
			delete(h.chatParticipants, key)
		}
	}

	// Send a message to client before closing (optional - they're being purged anyway)
	client.SendMessage(&WSMessage{
		Type: TypeError,
		Payload: ErrorPayload{
			Code:    "device_purged",
			Message: "Device has been purged",
		},
	})

	// Close the connection
	client.Close()

	fmt.Printf("[DEBUG] DisconnectDevice: %s fully disconnected and cleaned up\n", deviceUUID)
}

func (h *Hub) HandleMessage(client *Client, msg *WSMessage) {
	ctx := context.Background()

	fmt.Printf("[DEBUG] Received message type: %s\n", msg.Type)

	switch msg.Type {
	case TypeAuth:
		h.handleAuth(ctx, client, msg)
	case TypeChatRegister:
		h.handleChatRegister(ctx, client, msg)
	case TypeMessageSend:
		h.handleMessageSend(ctx, client, msg)
	case TypeMessageRead:
		h.handleMessageRead(ctx, client, msg)
	case TypeTypingStart, TypeTypingStop:
		h.handleTyping(ctx, client, msg)
	case TypePushRegister:
		h.handlePushRegister(ctx, client, msg)
	case TypePushUnregister:
		h.handlePushUnregister(ctx, client, msg)
	case TypePushBurnAll:
		h.handlePushBurnAll(ctx, client, msg)
	case "ping":
		return
	default:
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "unknown_type",
				Message: "Unknown message type",
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

	fmt.Printf("[DEBUG] Auth attempt from device: %s\n", payload.DeviceUUID)

	banned, reason, _ := h.redis.IsBanned(ctx, payload.DeviceUUID)
	if banned {
		fmt.Printf("[DEBUG] Device %s is banned: %s\n", payload.DeviceUUID, reason)
		client.SendMessage(&WSMessage{
			Type:    TypeBanned,
			Payload: BannedPayload{Reason: reason},
		})
		return
	}

	now := time.Now().Unix()
	if abs(now-payload.Timestamp) > 300 {
		fmt.Printf("[DEBUG] Auth failed: timestamp expired\n")
		client.SendMessage(&WSMessage{
			Type:    TypeAuthFailed,
			Payload: AuthFailedPayload{Reason: "timestamp_expired"},
		})
		return
	}

	publicKey, err := h.redis.GetDevicePublicKey(ctx, payload.DeviceUUID)
	if err != nil {
		fmt.Printf("[DEBUG] Auth failed: device not found - %v\n", err)
		client.SendMessage(&WSMessage{
			Type:    TypeAuthFailed,
			Payload: AuthFailedPayload{Reason: "device_not_found"},
		})
		return
	}

	expectedSig := computeSignature(publicKey, payload.DeviceUUID, payload.Timestamp)
	if payload.Signature != expectedSig {
		fmt.Printf("[DEBUG] Auth failed: invalid signature\n")
		client.SendMessage(&WSMessage{
			Type:    TypeAuthFailed,
			Payload: AuthFailedPayload{Reason: "invalid_signature"},
		})
		return
	}

	sub, err := h.redis.GetSubscription(ctx, payload.DeviceUUID)
	if err != nil || sub.Status != "active" || time.Now().After(sub.ExpiresAt) {
		fmt.Printf("[DEBUG] Auth failed: subscription expired or invalid\n")
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

	fmt.Printf("[DEBUG] Auth SUCCESS for device: %s (total clients: %d)\n", payload.DeviceUUID, len(h.clients))

	// Note: Chats are stored client-side, so we return empty list
	// Client will send chat.register with their local chats
	chats := make([]ChatInfo, 0)

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
}

// handleChatRegister validates and registers participant credentials for routing
func (h *Hub) handleChatRegister(ctx context.Context, client *Client, msg *WSMessage) {
	if !client.IsAuthed() {
		fmt.Printf("[DEBUG] chat.register rejected: not authenticated\n")
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
	var payload ChatRegisterPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		fmt.Printf("[DEBUG] chat.register rejected: invalid payload - %v\n", err)
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "invalid_payload",
				Message: "Invalid chat.register payload",
			},
		})
		return
	}

	deviceUUID := client.GetDeviceUUID()
	registered := 0
	failed := 0

	fmt.Printf("[DEBUG] ========================================\n")
	fmt.Printf("[DEBUG] chat.register from device: %s\n", deviceUUID)
	fmt.Printf("[DEBUG] Number of chats to register: %d\n", len(payload.Chats))

	h.mu.Lock()
	for _, chatReg := range payload.Chats {
		fmt.Printf("[DEBUG] ----------------------------------------\n")
		fmt.Printf("[DEBUG] Registering chat: %s\n", chatReg.ChatUUID)
		fmt.Printf("[DEBUG] Participant ID: %s\n", chatReg.ParticipantID)
		fmt.Printf("[DEBUG] Secret (first 8 chars): %.8s...\n", chatReg.ParticipantSecret)

		// Validate credentials against Redis
		valid, err := h.redis.ValidateParticipant(ctx, chatReg.ChatUUID, chatReg.ParticipantID, chatReg.ParticipantSecret)

		fmt.Printf("[DEBUG] Validation result: valid=%v, err=%v\n", valid, err)

		if err != nil || !valid {
			fmt.Printf("[DEBUG] FAILED to register chat %s\n", chatReg.ChatUUID)
			failed++
			continue
		}

		// Register mapping: chatUUID:participantID -> deviceUUID
		key := chatParticipantKey(chatReg.ChatUUID, chatReg.ParticipantID)
		h.chatParticipants[key] = deviceUUID
		registered++

		fmt.Printf("[DEBUG] SUCCESS: Mapped %s -> %s\n", key, deviceUUID)
	}
	h.mu.Unlock()

	fmt.Printf("[DEBUG] ========================================\n")
	fmt.Printf("[DEBUG] Registration complete: %d registered, %d failed\n", registered, failed)
	fmt.Printf("[DEBUG] Current chatParticipants map:\n")
	h.mu.RLock()
	for k, v := range h.chatParticipants {
		fmt.Printf("[DEBUG]   %s -> %s\n", k, v)
	}
	h.mu.RUnlock()
	fmt.Printf("[DEBUG] ========================================\n")

	// Deliver any queued messages for registered chats
	fmt.Printf("[DEBUG] Checking for queued messages...\n")
	for _, chatReg := range payload.Chats {
		fmt.Printf("[DEBUG] Checking queue for chat: %s\n", chatReg.ChatUUID)
		messages, err := h.redis.GetQueuedMessages(ctx, chatReg.ChatUUID)
		if err != nil {
			fmt.Printf("[DEBUG] Error getting queued messages: %v\n", err)
		}
		fmt.Printf("[DEBUG] Found %d queued messages for chat %s\n", len(messages), chatReg.ChatUUID)

		for msgID, queuedMsg := range messages {
			fmt.Printf("[DEBUG] Queued message %s from sender %s\n", msgID, queuedMsg.SenderParticipant)

			// Don't deliver own messages
			if queuedMsg.SenderParticipant == chatReg.ParticipantID {
				fmt.Printf("[DEBUG] Skipping own message %s\n", msgID)
				continue
			}

			fmt.Printf("[DEBUG] DELIVERING queued message %s to device %s\n", msgID, deviceUUID)

			err := client.SendMessage(&WSMessage{
				Type: TypeMessageReceived,
				Payload: MessageReceivedPayload{
					ChatUUID:         chatReg.ChatUUID,
					MessageID:        msgID,
					SenderUUID:       queuedMsg.SenderParticipant,
					SenderDeviceUUID: queuedMsg.SenderDeviceUUID,
					EncryptedContent: base64.StdEncoding.EncodeToString(queuedMsg.EncryptedContent),
					Timestamp:        time.Now().Unix(),
				},
			})
			if err != nil {
				fmt.Printf("[DEBUG] Error sending queued message: %v\n", err)
			} else {
				fmt.Printf("[DEBUG] Queued message sent successfully\n")
				// Notify sender that recipient received the message
				h.sendDeliveryConfirmation(ctx, chatReg.ChatUUID, msgID, queuedMsg.SenderParticipant)
			}
		}
	}
	fmt.Printf("[DEBUG] Finished checking queued messages\n")

	client.SendMessage(&WSMessage{
		Type: TypeChatRegisterAck,
		Payload: ChatRegisterAckPayload{
			Registered: registered,
			Failed:     failed,
		},
	})
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
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "invalid_payload",
				Message: "Invalid message payload",
			},
		})
		return
	}

	deviceUUID := client.GetDeviceUUID()

	fmt.Printf("[DEBUG] ========================================\n")
	fmt.Printf("[DEBUG] MESSAGE SEND from device: %s\n", deviceUUID)
	fmt.Printf("[DEBUG] Chat UUID: %s\n", payload.ChatUUID)
	fmt.Printf("[DEBUG] Sender Participant ID: %s\n", payload.ParticipantID)
	fmt.Printf("[DEBUG] Message ID: %s\n", payload.MessageID)

	// Rate limiting
	count, allowed, _ := h.redis.CheckRateLimit(ctx, deviceUUID, h.rateLimitPerMinute)
	if !allowed {
		fmt.Printf("[DEBUG] Rate limit exceeded for device %s\n", deviceUUID)
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

	// Validate sender's participant credentials
	valid, err := h.redis.ValidateParticipant(ctx, payload.ChatUUID, payload.ParticipantID, payload.ParticipantSecret)
	fmt.Printf("[DEBUG] Sender validation: valid=%v, err=%v\n", valid, err)

	if err != nil || !valid {
		fmt.Printf("[DEBUG] MESSAGE REJECTED: Invalid sender credentials\n")
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "invalid_credentials",
				Message: "Invalid participant credentials",
			},
		})
		return
	}

	// Get chat to find recipient's participant ID
	chat, err := h.redis.GetChat(ctx, payload.ChatUUID)
	if err != nil {
		fmt.Printf("[DEBUG] MESSAGE REJECTED: Chat not found - %v\n", err)
		client.SendMessage(&WSMessage{
			Type: TypeError,
			Payload: ErrorPayload{
				Code:    "chat_not_found",
				Message: "Chat not found",
			},
		})
		return
	}

	fmt.Printf("[DEBUG] Chat found: participant_a=%s, participant_b=%s\n", chat.ParticipantA, chat.ParticipantB)

	// Determine recipient's participant ID
	recipientParticipantID := chat.ParticipantA
	if chat.ParticipantA == payload.ParticipantID {
		recipientParticipantID = chat.ParticipantB
	}

	fmt.Printf("[DEBUG] Recipient participant ID: %s\n", recipientParticipantID)

	// Check if recipient is online via chatParticipants mapping
	h.mu.RLock()
	recipientKey := chatParticipantKey(payload.ChatUUID, recipientParticipantID)
	recipientDeviceUUID, recipientRegistered := h.chatParticipants[recipientKey]

	fmt.Printf("[DEBUG] Looking up recipient key: %s\n", recipientKey)
	fmt.Printf("[DEBUG] Recipient registered: %v\n", recipientRegistered)
	fmt.Printf("[DEBUG] Recipient device UUID: %s\n", recipientDeviceUUID)

	var recipient *Client
	var online bool
	if recipientRegistered {
		recipient, online = h.clients[recipientDeviceUUID]
		fmt.Printf("[DEBUG] Recipient online: %v, client exists: %v\n", online, recipient != nil)
	}

	// Debug: Print all current mappings
	fmt.Printf("[DEBUG] All chatParticipants mappings:\n")
	for k, v := range h.chatParticipants {
		fmt.Printf("[DEBUG]   %s -> %s\n", k, v)
	}
	fmt.Printf("[DEBUG] All connected clients:\n")
	for k := range h.clients {
		fmt.Printf("[DEBUG]   %s\n", k)
	}
	h.mu.RUnlock()

	content, err := base64.StdEncoding.DecodeString(payload.EncryptedContent)
	if err != nil || len(content) > 10240 {
		fmt.Printf("[DEBUG] MESSAGE REJECTED: Content too large or invalid base64\n")
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

	// Include sender's device UUID for Signal Protocol decryption
	outMsg := &WSMessage{
		Type: TypeMessageReceived,
		Payload: MessageReceivedPayload{
			ChatUUID:         payload.ChatUUID,
			MessageID:        payload.MessageID,
			SenderUUID:       payload.ParticipantID,
			SenderDeviceUUID: deviceUUID,
			EncryptedContent: payload.EncryptedContent,
			Timestamp:        time.Now().Unix(),
		},
	}

	if online && recipient != nil {
		fmt.Printf("[DEBUG] DELIVERING message to online recipient\n")
		recipient.SendMessage(outMsg)
		// Notify sender that recipient received the message immediately
		h.sendDeliveryConfirmation(ctx, payload.ChatUUID, payload.MessageID, payload.ParticipantID)
	} else {
		fmt.Printf("[DEBUG] QUEUING message (recipient offline or not registered)\n")
		// Queue message with sender's device UUID
		err := h.redis.QueueMessageWithDevice(ctx, payload.ChatUUID, payload.MessageID, payload.ParticipantID, deviceUUID, content)
		if err != nil {
			fmt.Printf("[DEBUG] ERROR queuing message: %v\n", err)
		} else {
			fmt.Printf("[DEBUG] Message queued successfully: chat=%s, msgID=%s\n", payload.ChatUUID, payload.MessageID)
		}
		// Always try to send push when recipient is offline
		h.sendPushNotification(ctx, recipientParticipantID, payload.ChatUUID)
	}

	// Send acknowledgment back to sender
	client.SendMessage(&WSMessage{
		Type: TypeMessageAck,
		Payload: MessageAckPayload{
			ChatUUID:  payload.ChatUUID,
			MessageID: payload.MessageID,
		},
	})
	fmt.Printf("[DEBUG] Sent message.ack for %s\n", payload.MessageID)

	fmt.Printf("[DEBUG] ========================================\n")
}

// sendPushNotification sends a BLIND wake-up push for a specific chat
// Uses participant ID to look up the FCM token (not device UUID)
func (h *Hub) sendPushNotification(ctx context.Context, recipientParticipantID, chatUUID string) {
	fmt.Printf("[DEBUG] PUSH: Attempting to send push for chat %s to participant %s\n", chatUUID, recipientParticipantID)

	if !firebase.IsInitialized() {
		fmt.Printf("[DEBUG] PUSH: Firebase NOT initialized - cannot send push\n")
		return
	}
	fmt.Printf("[DEBUG] PUSH: Firebase is initialized\n")

	// Get push token using participant ID
	fcmToken, err := h.redis.GetPushTokenForChat(ctx, chatUUID, recipientParticipantID)
	if err != nil {
		fmt.Printf("[DEBUG] PUSH: No FCM token found for chat %s, participant %s: %v\n", chatUUID, recipientParticipantID, err)
		return
	}
	fmt.Printf("[DEBUG] PUSH: Found FCM token: %.20s...\n", fcmToken)

	// BLIND WAKE-UP: No chat info in push payload
	// Prevents metadata leakage - server doesn't reveal which chat
	data := map[string]string{
		"type": "wake",
	}

	fmt.Printf("[DEBUG] PUSH: Sending push notification...\n")
	err = firebase.SendPush(ctx, fcmToken, data)
	if err != nil {
		fmt.Printf("[DEBUG] PUSH: Failed to send - %v\n", err)
	} else {
		fmt.Printf("[DEBUG] PUSH: Push sent successfully\n")
	}
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

	// Find sender's participant ID (whoever is not us)
	// We need to check who we are in this chat
	deviceUUID := client.GetDeviceUUID()

	// Find our participant ID
	var ourParticipantID, otherParticipantID string
	h.mu.RLock()
	for key, devUUID := range h.chatParticipants {
		if devUUID == deviceUUID {
			// Key is chatUUID:participantID
			if len(key) > len(payload.ChatUUID)+1 {
				ourParticipantID = key[len(payload.ChatUUID)+1:]
			}
		}
	}
	h.mu.RUnlock()

	if chat.ParticipantA == ourParticipantID {
		otherParticipantID = chat.ParticipantB
	} else {
		otherParticipantID = chat.ParticipantA
	}

	// Find other participant's device
	h.mu.RLock()
	otherKey := chatParticipantKey(payload.ChatUUID, otherParticipantID)
	otherDeviceUUID, found := h.chatParticipants[otherKey]
	var other *Client
	var online bool
	if found {
		other, online = h.clients[otherDeviceUUID]
	}
	h.mu.RUnlock()

	if online && other != nil {
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

	// Validate credentials
	valid, err := h.redis.ValidateParticipant(ctx, payload.ChatUUID, payload.ParticipantID, payload.ParticipantSecret)
	if err != nil || !valid {
		return
	}

	chat, err := h.redis.GetChat(ctx, payload.ChatUUID)
	if err != nil {
		return
	}

	// Find other participant
	otherParticipantID := chat.ParticipantA
	if chat.ParticipantA == payload.ParticipantID {
		otherParticipantID = chat.ParticipantB
	}

	h.mu.RLock()
	otherKey := chatParticipantKey(payload.ChatUUID, otherParticipantID)
	otherDeviceUUID, found := h.chatParticipants[otherKey]
	var other *Client
	var online bool
	if found {
		other, online = h.clients[otherDeviceUUID]
	}
	h.mu.RUnlock()

	if online && other != nil {
		other.SendMessage(&WSMessage{
			Type: TypeTypingIndicator,
			Payload: TypingPayload{
				ChatUUID: payload.ChatUUID,
			},
		})
	}
}

// sendDeliveryConfirmation notifies sender that recipient received their message
func (h *Hub) sendDeliveryConfirmation(ctx context.Context, chatUUID, messageID, senderParticipantID string) {
	h.mu.RLock()
	senderKey := chatParticipantKey(chatUUID, senderParticipantID)
	senderDeviceUUID, found := h.chatParticipants[senderKey]
	var senderClient *Client
	var online bool
	if found {
		senderClient, online = h.clients[senderDeviceUUID]
	}
	h.mu.RUnlock()

	if online && senderClient != nil {
		senderClient.SendMessage(&WSMessage{
			Type: TypeMessageDelivered,
			Payload: MessageDeliveredPayload{
				ChatUUID:  chatUUID,
				MessageID: messageID,
			},
		})
		fmt.Printf("[DEBUG] Sent message.delivered to sender for %s\n", messageID)
	}
}

// handlePushRegister registers an FCM token for a specific chat
// Token is stored per-chat using participant ID (not device UUID)
// NOTE: Does NOT require client.IsAuthed() because validation is done via payload credentials
// This allows push registration to succeed even if client disconnects during processing
func (h *Hub) handlePushRegister(ctx context.Context, client *Client, msg *WSMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload PushRegisterPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		fmt.Printf("[DEBUG] PUSH REGISTER: Invalid payload - %v\n", err)
		// Don't send response - client may have disconnected
		return
	}

	fmt.Printf("[DEBUG] PUSH REGISTER: chat=%s participant=%s token=%.20s...\n",
		payload.ChatUUID, payload.ParticipantID, payload.FCMToken)

	// Validate using credentials from payload (not client auth state)
	if payload.ParticipantID == "" || payload.ParticipantSecret == "" {
		fmt.Printf("[DEBUG] PUSH REGISTER: Missing credentials in payload\n")
		return
	}

	valid, err := h.redis.ValidateParticipant(ctx, payload.ChatUUID, payload.ParticipantID, payload.ParticipantSecret)
	if err != nil || !valid {
		fmt.Printf("[DEBUG] PUSH REGISTER: Invalid credentials - valid=%v err=%v\n", valid, err)
		return
	}

	// Register push token using participant ID from payload
	err = h.redis.RegisterPushForChat(ctx, payload.ChatUUID, payload.ParticipantID, payload.FCMToken)

	if err != nil {
		fmt.Printf("[DEBUG] PUSH REGISTER: Failed to store token - %v\n", err)
	} else {
		fmt.Printf("[DEBUG] PUSH REGISTER: Success - token stored in Redis\n")
	}

	// Try to send ack, but don't fail if client disconnected
	if client.IsAuthed() {
		client.SendMessage(&WSMessage{
			Type: TypePushRegisterAck,
			Payload: PushRegisterAckPayload{
				ChatUUID: payload.ChatUUID,
				Success:  err == nil,
			},
		})
	}
}

// handlePushUnregister removes push registration for a specific chat
// NOTE: Does NOT require client.IsAuthed() because validation is done via payload credentials
func (h *Hub) handlePushUnregister(ctx context.Context, client *Client, msg *WSMessage) {
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload PushUnregisterPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return
	}

	fmt.Printf("[DEBUG] PUSH UNREGISTER: chat=%s participant=%s\n", payload.ChatUUID, payload.ParticipantID)

	// Validate using credentials from payload
	if payload.ParticipantID == "" || payload.ParticipantSecret == "" {
		fmt.Printf("[DEBUG] PUSH UNREGISTER: Missing credentials in payload\n")
		return
	}

	valid, err := h.redis.ValidateParticipant(ctx, payload.ChatUUID, payload.ParticipantID, payload.ParticipantSecret)
	if err != nil || !valid {
		fmt.Printf("[DEBUG] PUSH UNREGISTER: Invalid credentials\n")
		return
	}

	// Remove push token using participant ID from payload
	err = h.redis.DeletePushForChat(ctx, payload.ChatUUID, payload.ParticipantID)

	if err != nil {
		fmt.Printf("[DEBUG] PUSH UNREGISTER: Failed - %v\n", err)
	} else {
		fmt.Printf("[DEBUG] PUSH UNREGISTER: Success\n")
	}

	// Try to send ack if client still connected
	if client.IsAuthed() {
		client.SendMessage(&WSMessage{
			Type: TypePushUnregisterAck,
			Payload: PushUnregisterAckPayload{
				ChatUUID: payload.ChatUUID,
				Success:  err == nil,
			},
		})
	}
}

// handlePushBurnAll removes ALL push registrations for specified participant IDs
// Called when FCM token rotates - all previous registrations are invalid
func (h *Hub) handlePushBurnAll(ctx context.Context, client *Client, msg *WSMessage) {
	if !client.IsAuthed() {
		return
	}

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload PushBurnAllPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		client.SendMessage(&WSMessage{
			Type: TypePushBurnAllAck,
			Payload: PushBurnAllAckPayload{
				Deleted: 0,
			},
		})
		return
	}

	fmt.Printf("[DEBUG] PUSH BURN ALL: participant_ids=%v\n", payload.ParticipantIDs)

	// Delete all push registrations for these participant IDs
	deleted, err := h.redis.DeleteAllPushForDevice(ctx, payload.ParticipantIDs)
	if err != nil {
		deleted = 0
	}

	fmt.Printf("[DEBUG] PUSH BURN ALL: deleted=%d\n", deleted)

	client.SendMessage(&WSMessage{
		Type: TypePushBurnAllAck,
		Payload: PushBurnAllAckPayload{
			Deleted: int(deleted),
		},
	})
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

	// Use participant mapping to find clients
	keyA := chatParticipantKey(chatUUID, chat.ParticipantA)
	keyB := chatParticipantKey(chatUUID, chat.ParticipantB)

	if deviceA, ok := h.chatParticipants[keyA]; ok {
		if clientA, ok := h.clients[deviceA]; ok {
			clientA.SendMessage(msg)
		}
	}
	if deviceB, ok := h.chatParticipants[keyB]; ok {
		if clientB, ok := h.clients[deviceB]; ok {
			clientB.SendMessage(msg)
		}
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