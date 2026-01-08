package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	MaxChatTTL = 5 * time.Minute
)

type Chat struct {
	ChatUUID           string    `json:"chat_uuid"`
	ParticipantA       string    `json:"participant_a"`
	ParticipantASecret string    `json:"participant_a_secret"`
	ParticipantADevice string    `json:"participant_a_device"` // Device UUID for participant A
	ParticipantB       string    `json:"participant_b"`
	ParticipantBSecret string    `json:"participant_b_secret"`
	ParticipantBDevice string    `json:"participant_b_device"` // Device UUID for participant B
	TTLSeconds         int       `json:"ttl_seconds"`
	CreatedAt          time.Time `json:"created_at"`
	Status             string    `json:"status"`
}

type ChatInvitation struct {
	Token           string    `json:"token"`
	ChatUUID        string    `json:"chat_uuid"`
	CreatorDeviceID string    `json:"creator_device_id"`
	TTLSeconds      int       `json:"ttl_seconds"`
	CreatedAt       time.Time `json:"created_at"`
	Used            bool      `json:"used"`
}

type QueuedMessage struct {
	SenderParticipant string `json:"sender_participant"`
	SenderDeviceUUID  string `json:"sender_device_uuid"`
	EncryptedContent  []byte `json:"encrypted_content"`
}

func HashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func (c *Client) ValidateParticipant(ctx context.Context, chatUUID, participantID, secret string) (bool, error) {
	chat, err := c.GetChat(ctx, chatUUID)
	if err != nil {
		return false, err
	}
	secretHash := HashSecret(secret)
	if chat.ParticipantA == participantID {
		return chat.ParticipantASecret == secretHash, nil
	}
	if chat.ParticipantB == participantID {
		return chat.ParticipantBSecret == secretHash, nil
	}
	return false, fmt.Errorf("participant not found in chat")
}

// IsDeviceParticipant checks if a device UUID is a participant in the chat
func (c *Client) IsDeviceParticipant(ctx context.Context, chatUUID, deviceUUID string) (bool, string, error) {
	chat, err := c.GetChat(ctx, chatUUID)
	if err != nil {
		return false, "", err
	}
	if chat.ParticipantADevice == deviceUUID {
		return true, chat.ParticipantA, nil
	}
	if chat.ParticipantBDevice == deviceUUID {
		return true, chat.ParticipantB, nil
	}
	return false, "", nil
}

// GetOtherParticipantDevice returns the other participant's device UUID
func (c *Client) GetOtherParticipantDevice(ctx context.Context, chatUUID, deviceUUID string) (string, error) {
	chat, err := c.GetChat(ctx, chatUUID)
	if err != nil {
		return "", err
	}
	if chat.ParticipantADevice == deviceUUID {
		return chat.ParticipantBDevice, nil
	}
	if chat.ParticipantBDevice == deviceUUID {
		return chat.ParticipantADevice, nil
	}
	return "", fmt.Errorf("device not in chat")
}

func (c *Client) CreateChat(ctx context.Context, chatUUID, participantID, participantSecret, creatorDeviceID, invitationToken string, ttlSeconds int) error {
	secretHash := HashSecret(participantSecret)
	chat := Chat{
		ChatUUID:           chatUUID,
		ParticipantA:       participantID,
		ParticipantASecret: secretHash,
		ParticipantADevice: creatorDeviceID, // Store creator's device UUID
		ParticipantB:       "",
		ParticipantBSecret: "",
		ParticipantBDevice: "",
		TTLSeconds:         ttlSeconds,
		CreatedAt:          time.Now(),
		Status:             "pending",
	}
	chatJSON, err := json.Marshal(chat)
	if err != nil {
		return fmt.Errorf("failed to marshal chat: %w", err)
	}
	chatKey := fmt.Sprintf("chat:%s", chatUUID)
	if err := c.rdb.Set(ctx, chatKey, chatJSON, 24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to store chat: %w", err)
	}
	invitation := ChatInvitation{
		Token:           invitationToken,
		ChatUUID:        chatUUID,
		CreatorDeviceID: creatorDeviceID,
		TTLSeconds:      ttlSeconds,
		CreatedAt:       time.Now(),
		Used:            false,
	}
	invJSON, err := json.Marshal(invitation)
	if err != nil {
		return fmt.Errorf("failed to marshal invitation: %w", err)
	}
	invKey := fmt.Sprintf("invite:%s", invitationToken)
	if err := c.rdb.Set(ctx, invKey, invJSON, 24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to store invitation: %w", err)
	}
	return nil
}

func (c *Client) GetChat(ctx context.Context, chatUUID string) (*Chat, error) {
	chatKey := fmt.Sprintf("chat:%s", chatUUID)
	chatJSON, err := c.rdb.Get(ctx, chatKey).Result()
	if err != nil {
		return nil, fmt.Errorf("chat not found: %w", err)
	}
	var chat Chat
	if err := json.Unmarshal([]byte(chatJSON), &chat); err != nil {
		return nil, fmt.Errorf("failed to unmarshal chat: %w", err)
	}
	return &chat, nil
}

func (c *Client) GetInvitation(ctx context.Context, token string) (*ChatInvitation, error) {
	invKey := fmt.Sprintf("invite:%s", token)
	invJSON, err := c.rdb.Get(ctx, invKey).Result()
	if err != nil {
		return nil, fmt.Errorf("invitation not found: %w", err)
	}
	var invitation ChatInvitation
	if err := json.Unmarshal([]byte(invJSON), &invitation); err != nil {
		return nil, fmt.Errorf("failed to unmarshal invitation: %w", err)
	}
	return &invitation, nil
}

// JoinChat now also stores the joiner's device UUID
func (c *Client) JoinChat(ctx context.Context, token, joinerDeviceUUID, participantID, participantSecret string) (*Chat, string, error) {
	invKey := fmt.Sprintf("invite:%s", token)
	secretHash := HashSecret(participantSecret)
	joinScript := `
		local invKey = KEYS[1]
		local joinerDevice = ARGV[1]
		local participantID = ARGV[2]
		local secretHash = ARGV[3]
		local invJSON = redis.call('GET', invKey)
		if not invJSON then
			return {-1, "", ""}
		end
		local inv = cjson.decode(invJSON)
		if inv.used then
			return {-2, "", ""}
		end
		local chatKey = 'chat:' .. inv.chat_uuid
		local chatJSON = redis.call('GET', chatKey)
		if not chatJSON then
			return {-1, "", ""}
		end
		local chat = cjson.decode(chatJSON)
		if chat.status ~= 'pending' then
			return {-4, "", ""}
		end
		if chat.participant_a == participantID then
			return {-3, "", ""}
		end
		chat.participant_b = participantID
		chat.participant_b_secret = secretHash
		chat.participant_b_device = joinerDevice
		chat.status = 'active'
		redis.call('SET', chatKey, cjson.encode(chat))
		inv.used = true
		redis.call('SET', invKey, cjson.encode(inv), 'EX', 3600)
		return {1, cjson.encode(chat), inv.creator_device_id}
	`
	result, err := c.rdb.Eval(ctx, joinScript, []string{invKey}, joinerDeviceUUID, participantID, secretHash).Result()
	if err != nil {
		return nil, "", fmt.Errorf("failed to execute join script: %w", err)
	}
	arr, ok := result.([]interface{})
	if !ok || len(arr) < 1 {
		return nil, "", fmt.Errorf("invalid script result")
	}
	code, _ := arr[0].(int64)
	switch code {
	case -1:
		return nil, "", fmt.Errorf("invitation not found")
	case -2:
		return nil, "", fmt.Errorf("invitation already used")
	case -3:
		return nil, "", fmt.Errorf("cannot join with same participant ID")
	case -4:
		return nil, "", fmt.Errorf("chat is not pending")
	case 1:
		if len(arr) < 3 {
			return nil, "", fmt.Errorf("invalid script result")
		}
		chatJSON, _ := arr[1].(string)
		creatorDeviceID, _ := arr[2].(string)
		var chat Chat
		if err := json.Unmarshal([]byte(chatJSON), &chat); err != nil {
			return nil, "", fmt.Errorf("failed to parse chat: %w", err)
		}
		return &chat, creatorDeviceID, nil
	default:
		return nil, "", fmt.Errorf("unknown error")
	}
}

func (c *Client) GetUserChats(ctx context.Context, deviceUUID string) ([]string, error) {
	return []string{}, nil
}

func (c *Client) DeleteChat(ctx context.Context, chatUUID string) error {
	chatKey := fmt.Sprintf("chat:%s", chatUUID)
	if err := c.rdb.Del(ctx, chatKey).Err(); err != nil {
		return fmt.Errorf("failed to delete chat: %w", err)
	}
	return nil
}

func (c *Client) QueueMessage(ctx context.Context, chatUUID, messageID, senderParticipant string, encryptedContent []byte) error {
	return c.QueueMessageWithDevice(ctx, chatUUID, messageID, senderParticipant, "", encryptedContent)
}

func (c *Client) QueueMessageWithDevice(ctx context.Context, chatUUID, messageID, senderParticipant, senderDeviceUUID string, encryptedContent []byte) error {
	msg := QueuedMessage{
		SenderParticipant: senderParticipant,
		SenderDeviceUUID:  senderDeviceUUID,
		EncryptedContent:  encryptedContent,
	}
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	msgKey := fmt.Sprintf("msg:%s:%s", chatUUID, messageID)
	if err := c.rdb.Set(ctx, msgKey, msgJSON, MaxChatTTL).Err(); err != nil {
		return fmt.Errorf("failed to queue message: %w", err)
	}
	queueKey := fmt.Sprintf("msg_queue:%s", chatUUID)
	if err := c.rdb.RPush(ctx, queueKey, messageID).Err(); err != nil {
		return fmt.Errorf("failed to add to message queue: %w", err)
	}
	c.rdb.Expire(ctx, queueKey, MaxChatTTL)
	return nil
}

func (c *Client) GetQueuedMessages(ctx context.Context, chatUUID string) (map[string]*QueuedMessage, error) {
	queueKey := fmt.Sprintf("msg_queue:%s", chatUUID)
	messageIDs, err := c.rdb.LRange(ctx, queueKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	messages := make(map[string]*QueuedMessage)
	for _, msgID := range messageIDs {
		msgKey := fmt.Sprintf("msg:%s:%s", chatUUID, msgID)
		content, err := c.rdb.Get(ctx, msgKey).Bytes()
		if err == nil {
			var msg QueuedMessage
			if json.Unmarshal(content, &msg) == nil {
				messages[msgID] = &msg
			}
		}
	}
	return messages, nil
}

func (c *Client) DeleteQueuedMessage(ctx context.Context, chatUUID, messageID string) error {
	msgKey := fmt.Sprintf("msg:%s:%s", chatUUID, messageID)
	c.rdb.Del(ctx, msgKey)
	queueKey := fmt.Sprintf("msg_queue:%s", chatUUID)
	c.rdb.LRem(ctx, queueKey, 1, messageID)
	return nil
}

func (c *Client) StoreParticipantFCM(ctx context.Context, chatUUID, participantID, fcmToken string) error {
	key := fmt.Sprintf("fcm:%s:%s", chatUUID, participantID)
	return c.rdb.Set(ctx, key, fcmToken, 24*time.Hour).Err()
}

func (c *Client) GetParticipantFCM(ctx context.Context, chatUUID, participantID string) (string, error) {
	key := fmt.Sprintf("fcm:%s:%s", chatUUID, participantID)
	return c.rdb.Get(ctx, key).Result()
}

func (c *Client) DeleteParticipantFCM(ctx context.Context, chatUUID, participantID string) error {
	key := fmt.Sprintf("fcm:%s:%s", chatUUID, participantID)
	return c.rdb.Del(ctx, key).Err()
}