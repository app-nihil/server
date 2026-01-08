package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// PushRegistration represents a chat-scoped push token
// Key: push:{chat_uuid}:{participant_id}
// Using participant ID (not device UUID) so we can look up tokens for offline users
type PushRegistration struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

// RegisterPushForChat stores a push token for a specific chat participant
// participantID is the user's participant ID for this chat (not device UUID)
func (c *Client) RegisterPushForChat(ctx context.Context, chatUUID, participantID, fcmToken string) error {
	// Get chat to verify it exists and participant is valid
	chat, err := c.GetChat(ctx, chatUUID)
	if err != nil {
		return fmt.Errorf("chat not found: %w", err)
	}

	// Verify participant is in this chat
	if chat.ParticipantA != participantID && chat.ParticipantB != participantID {
		return fmt.Errorf("participant not in chat")
	}

	reg := PushRegistration{
		Token:     fcmToken,
		CreatedAt: time.Now(),
	}

	regJSON, err := json.Marshal(reg)
	if err != nil {
		return fmt.Errorf("failed to marshal push registration: %w", err)
	}

	// Use 24h TTL (same as chat expiry)
	ttl := 24 * time.Hour

	key := fmt.Sprintf("push:%s:%s", chatUUID, participantID)
	if err := c.rdb.Set(ctx, key, regJSON, ttl).Err(); err != nil {
		return fmt.Errorf("failed to store push registration: %w", err)
	}

	return nil
}

// GetPushTokenForChat retrieves a push token for a specific chat participant
// participantID is the participant ID (not device UUID)
func (c *Client) GetPushTokenForChat(ctx context.Context, chatUUID, participantID string) (string, error) {
	key := fmt.Sprintf("push:%s:%s", chatUUID, participantID)

	regJSON, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return "", fmt.Errorf("push registration not found: %w", err)
	}

	var reg PushRegistration
	if err := json.Unmarshal([]byte(regJSON), &reg); err != nil {
		return "", fmt.Errorf("failed to unmarshal push registration: %w", err)
	}

	return reg.Token, nil
}

// DeletePushForChat removes push registration for a specific chat participant
func (c *Client) DeletePushForChat(ctx context.Context, chatUUID, participantID string) error {
	key := fmt.Sprintf("push:%s:%s", chatUUID, participantID)
	return c.rdb.Del(ctx, key).Err()
}

// DeleteAllPushForParticipant removes push registrations matching a participant pattern
// This is tricky because participant IDs are per-chat, so we need to search
func (c *Client) DeleteAllPushForParticipant(ctx context.Context, participantID string) (int64, error) {
	// Find all push registrations for this participant
	pattern := fmt.Sprintf("push:*:%s", participantID)

	keys, err := c.rdb.Keys(ctx, pattern).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to find push registrations: %w", err)
	}

	if len(keys) == 0 {
		return 0, nil
	}

	// Delete all found keys
	deleted, err := c.rdb.Del(ctx, keys...).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to delete push registrations: %w", err)
	}

	return deleted, nil
}

// DeleteAllPushForDevice removes ALL push registrations for all chats a device has registered
// Called on: token refresh, app restart
// Since we now key by participant ID, we need the participant IDs to delete
// This function now takes a list of participant IDs that belong to the device
func (c *Client) DeleteAllPushForDevice(ctx context.Context, participantIDs []string) (int64, error) {
	if len(participantIDs) == 0 {
		return 0, nil
	}

	var totalDeleted int64
	for _, participantID := range participantIDs {
		pattern := fmt.Sprintf("push:*:%s", participantID)
		keys, err := c.rdb.Keys(ctx, pattern).Result()
		if err != nil {
			continue
		}
		if len(keys) > 0 {
			deleted, _ := c.rdb.Del(ctx, keys...).Result()
			totalDeleted += deleted
		}
	}

	return totalDeleted, nil
}

// DeleteAllPushForChat removes ALL push registrations for a chat
// Called when chat expires or is deleted
func (c *Client) DeleteAllPushForChat(ctx context.Context, chatUUID string) error {
	pattern := fmt.Sprintf("push:%s:*", chatUUID)

	keys, err := c.rdb.Keys(ctx, pattern).Result()
	if err != nil {
		return fmt.Errorf("failed to find push registrations: %w", err)
	}

	if len(keys) == 0 {
		return nil
	}

	return c.rdb.Del(ctx, keys...).Err()
}