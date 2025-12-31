package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	MaxChatTTL = 5 * time.Minute
)

type Chat struct {
	ChatUUID     string    `json:"chat_uuid"`
	ParticipantA string    `json:"participant_a"`
	ParticipantB string    `json:"participant_b"`
	TTLSeconds   int       `json:"ttl_seconds"`
	CreatedAt    time.Time `json:"created_at"`
	Status       string    `json:"status"`
}

type ChatInvitation struct {
	Token       string    `json:"token"`
	ChatUUID    string    `json:"chat_uuid"`
	CreatorUUID string    `json:"creator_uuid"`
	TTLSeconds  int       `json:"ttl_seconds"`
	CreatedAt   time.Time `json:"created_at"`
	Used        bool      `json:"used"`
}

type QueuedMessage struct {
	SenderUUID       string `json:"sender_uuid"`
	EncryptedContent []byte `json:"encrypted_content"`
}

func (c *Client) CreateChat(ctx context.Context, chatUUID, creatorUUID, invitationToken string, ttlSeconds int) error {
	chat := Chat{
		ChatUUID:     chatUUID,
		ParticipantA: creatorUUID,
		ParticipantB: "",
		TTLSeconds:   ttlSeconds,
		CreatedAt:    time.Now(),
		Status:       "pending",
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
		Token:       invitationToken,
		ChatUUID:    chatUUID,
		CreatorUUID: creatorUUID,
		TTLSeconds:  ttlSeconds,
		CreatedAt:   time.Now(),
		Used:        false,
	}

	invJSON, err := json.Marshal(invitation)
	if err != nil {
		return fmt.Errorf("failed to marshal invitation: %w", err)
	}

	invKey := fmt.Sprintf("invite:%s", invitationToken)
	if err := c.rdb.Set(ctx, invKey, invJSON, 24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to store invitation: %w", err)
	}

	userChatsKey := fmt.Sprintf("user_chats:%s", creatorUUID)
	if err := c.rdb.SAdd(ctx, userChatsKey, chatUUID).Err(); err != nil {
		return fmt.Errorf("failed to add chat to user list: %w", err)
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

// JoinChat atomically joins a chat using a Lua script to prevent race conditions
// This ensures only one user can join a pending chat
func (c *Client) JoinChat(ctx context.Context, token, joinerUUID string) (*Chat, error) {
	invKey := fmt.Sprintf("invite:%s", token)

	// Lua script for atomic join
	// Returns: 1=success, -1=not found, -2=already used, -3=self join, -4=not pending
	joinScript := `
		local invKey = KEYS[1]
		local joinerUUID = ARGV[1]
		
		-- Get invitation
		local invJSON = redis.call('GET', invKey)
		if not invJSON then
			return {-1, "", ""}
		end
		
		local inv = cjson.decode(invJSON)
		
		-- Check if already used
		if inv.used then
			return {-2, "", ""}
		end
		
		-- Check self-join
		if inv.creator_uuid == joinerUUID then
			return {-3, "", ""}
		end
		
		-- Get chat
		local chatKey = 'chat:' .. inv.chat_uuid
		local chatJSON = redis.call('GET', chatKey)
		if not chatJSON then
			return {-1, "", ""}
		end
		
		local chat = cjson.decode(chatJSON)
		
		-- Check if pending
		if chat.status ~= 'pending' then
			return {-4, "", ""}
		end
		
		-- Update chat
		chat.participant_b = joinerUUID
		chat.status = 'active'
		redis.call('SET', chatKey, cjson.encode(chat))
		
		-- Mark invitation as used
		inv.used = true
		redis.call('SET', invKey, cjson.encode(inv), 'EX', 3600)
		
		-- Add to joiner's chat list
		local userChatsKey = 'user_chats:' .. joinerUUID
		redis.call('SADD', userChatsKey, inv.chat_uuid)
		
		return {1, cjson.encode(chat), inv.chat_uuid}
	`

	result, err := c.rdb.Eval(ctx, joinScript, []string{invKey}, joinerUUID).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to execute join script: %w", err)
	}

	// Parse result
	arr, ok := result.([]interface{})
	if !ok || len(arr) < 1 {
		return nil, fmt.Errorf("invalid script result")
	}

	code, _ := arr[0].(int64)
	switch code {
	case -1:
		return nil, fmt.Errorf("invitation not found")
	case -2:
		return nil, fmt.Errorf("invitation already used")
	case -3:
		return nil, fmt.Errorf("cannot join your own chat")
	case -4:
		return nil, fmt.Errorf("chat is not pending")
	case 1:
		// Success - parse chat from result
		if len(arr) < 2 {
			return nil, fmt.Errorf("invalid script result")
		}
		chatJSON, _ := arr[1].(string)
		var chat Chat
		if err := json.Unmarshal([]byte(chatJSON), &chat); err != nil {
			return nil, fmt.Errorf("failed to parse chat: %w", err)
		}
		return &chat, nil
	default:
		return nil, fmt.Errorf("unknown error")
	}
}

func (c *Client) GetUserChats(ctx context.Context, deviceUUID string) ([]string, error) {
	userChatsKey := fmt.Sprintf("user_chats:%s", deviceUUID)
	return c.rdb.SMembers(ctx, userChatsKey).Result()
}

func (c *Client) DeleteChat(ctx context.Context, chatUUID string) error {
	chat, err := c.GetChat(ctx, chatUUID)
	if err != nil {
		return err
	}

	chatKey := fmt.Sprintf("chat:%s", chatUUID)
	if err := c.rdb.Del(ctx, chatKey).Err(); err != nil {
		return fmt.Errorf("failed to delete chat: %w", err)
	}

	if chat.ParticipantA != "" {
		userChatsKeyA := fmt.Sprintf("user_chats:%s", chat.ParticipantA)
		c.rdb.SRem(ctx, userChatsKeyA, chatUUID)
	}
	if chat.ParticipantB != "" {
		userChatsKeyB := fmt.Sprintf("user_chats:%s", chat.ParticipantB)
		c.rdb.SRem(ctx, userChatsKeyB, chatUUID)
	}

	return nil
}

func (c *Client) QueueMessage(ctx context.Context, chatUUID, messageID, senderUUID string, encryptedContent []byte) error {
	msg := QueuedMessage{
		SenderUUID:       senderUUID,
		EncryptedContent: encryptedContent,
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