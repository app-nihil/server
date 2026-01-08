package redis

import (
	"context"
	"testing"
	"time"
)

// To run these tests, you need Redis running locally:
// docker run -d -p 6379:6379 redis:7-alpine
//
// Then run:
// go test ./internal/redis -run TestJoinChat -v

func setupTestClient(t *testing.T) *Client {
	client, err := NewClient("redis://localhost:6379")
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	return client
}

func TestJoinChat_Success(t *testing.T) {
	client := setupTestClient(t)
	ctx := context.Background()

	// Create a chat
	chatUUID := "test-chat-" + time.Now().Format("150405")
	creatorUUID := "creator-device-123"
	joinerUUID := "joiner-device-456"
	invitationToken := "test-token-" + time.Now().Format("150405")

	err := client.CreateChat(ctx, chatUUID, creatorUUID, invitationToken, 60)
	if err != nil {
		t.Fatalf("Failed to create chat: %v", err)
	}

	// Join the chat
	chat, err := client.JoinChat(ctx, invitationToken, joinerUUID)
	if err != nil {
		t.Fatalf("Failed to join chat: %v", err)
	}

	// Verify
	if chat.Status != "active" {
		t.Errorf("Expected status 'active', got '%s'", chat.Status)
	}
	if chat.ParticipantB != joinerUUID {
		t.Errorf("Expected participant_b '%s', got '%s'", joinerUUID, chat.ParticipantB)
	}

	t.Logf("✓ Join successful: chat=%s, status=%s", chat.ChatUUID, chat.Status)

	// Cleanup
	client.DeleteChat(ctx, chatUUID)
}

func TestJoinChat_AlreadyUsed(t *testing.T) {
	client := setupTestClient(t)
	ctx := context.Background()

	// Create a chat
	chatUUID := "test-chat-used-" + time.Now().Format("150405")
	creatorUUID := "creator-device-123"
	joinerUUID1 := "joiner-device-456"
	joinerUUID2 := "joiner-device-789"
	invitationToken := "test-token-used-" + time.Now().Format("150405")

	err := client.CreateChat(ctx, chatUUID, creatorUUID, invitationToken, 60)
	if err != nil {
		t.Fatalf("Failed to create chat: %v", err)
	}

	// First join - should succeed
	_, err = client.JoinChat(ctx, invitationToken, joinerUUID1)
	if err != nil {
		t.Fatalf("First join failed: %v", err)
	}
	t.Log("✓ First join successful")

	// Second join - should fail
	_, err = client.JoinChat(ctx, invitationToken, joinerUUID2)
	if err == nil {
		t.Fatal("Second join should have failed but didn't")
	}

	t.Logf("✓ Second join correctly rejected: %v", err)

	// Cleanup
	client.DeleteChat(ctx, chatUUID)
}

func TestJoinChat_SelfJoin(t *testing.T) {
	client := setupTestClient(t)
	ctx := context.Background()

	// Create a chat
	chatUUID := "test-chat-self-" + time.Now().Format("150405")
	creatorUUID := "creator-device-123"
	invitationToken := "test-token-self-" + time.Now().Format("150405")

	err := client.CreateChat(ctx, chatUUID, creatorUUID, invitationToken, 60)
	if err != nil {
		t.Fatalf("Failed to create chat: %v", err)
	}

	// Try to join own chat - should fail
	_, err = client.JoinChat(ctx, invitationToken, creatorUUID)
	if err == nil {
		t.Fatal("Self-join should have failed but didn't")
	}

	t.Logf("✓ Self-join correctly rejected: %v", err)

	// Cleanup
	client.DeleteChat(ctx, chatUUID)
}

func TestJoinChat_InvalidToken(t *testing.T) {
	client := setupTestClient(t)
	ctx := context.Background()

	// Try to join with invalid token
	_, err := client.JoinChat(ctx, "nonexistent-token", "some-device")
	if err == nil {
		t.Fatal("Join with invalid token should have failed")
	}

	t.Logf("✓ Invalid token correctly rejected: %v", err)
}

func TestJoinChat_RaceCondition(t *testing.T) {
	client := setupTestClient(t)
	ctx := context.Background()

	// Create a chat
	chatUUID := "test-chat-race-" + time.Now().Format("150405")
	creatorUUID := "creator-device-123"
	invitationToken := "test-token-race-" + time.Now().Format("150405")

	err := client.CreateChat(ctx, chatUUID, creatorUUID, invitationToken, 60)
	if err != nil {
		t.Fatalf("Failed to create chat: %v", err)
	}

	// Simulate race condition - 10 concurrent joins
	results := make(chan error, 10)

	for i := 0; i < 10; i++ {
		go func(deviceNum int) {
			joinerUUID := "joiner-" + string(rune('A'+deviceNum))
			_, err := client.JoinChat(ctx, invitationToken, joinerUUID)
			results <- err
		}(i)
	}

	// Count successes
	successCount := 0
	for i := 0; i < 10; i++ {
		err := <-results
		if err == nil {
			successCount++
		}
	}

	// Only 1 should succeed
	if successCount != 1 {
		t.Errorf("Expected exactly 1 successful join, got %d", successCount)
	}

	t.Logf("✓ Race condition test passed: %d/10 succeeded (expected 1)", successCount)

	// Cleanup
	client.DeleteChat(ctx, chatUUID)
}