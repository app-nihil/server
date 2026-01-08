package firebase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2/google"
)

type Client struct {
	projectID  string
	httpClient *http.Client
	token      *google.Credentials
}

type FCMMessage struct {
	Message Message `json:"message"`
}

type Message struct {
	Token        string            `json:"token"`
	Notification *Notification     `json:"notification,omitempty"`
	Data         map[string]string `json:"data,omitempty"`
	Android      *AndroidConfig    `json:"android,omitempty"`
}

type Notification struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

type AndroidConfig struct {
	Priority string `json:"priority,omitempty"`
}

var client *Client

// Initialize creates the Firebase client
// serviceAccountJSON is the content of the service account JSON file
func Initialize(projectID string, serviceAccountJSON []byte) error {
	ctx := context.Background()
	
	creds, err := google.CredentialsFromJSON(ctx, serviceAccountJSON,
		"https://www.googleapis.com/auth/firebase.messaging",
	)
	if err != nil {
		return fmt.Errorf("failed to create credentials: %w", err)
	}

	client = &Client{
		projectID:  projectID,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		token:      creds,
	}

	return nil
}

// SendPush sends a push notification that shows even when app is closed
func SendPush(ctx context.Context, fcmToken string, data map[string]string) error {
	if client == nil {
		return fmt.Errorf("firebase client not initialized")
	}

	// Get OAuth2 token
	token, err := client.token.TokenSource.Token()
	if err != nil {
		return fmt.Errorf("failed to get token: %w", err)
	}

	msg := FCMMessage{
		Message: Message{
			Token: fcmToken,
			// Notification field is required for background/closed app
			Notification: &Notification{
				Title: "nihil",
				Body:  "New message",
			},
			Data: data,
			Android: &AndroidConfig{
				Priority: "high",
			},
		},
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", client.projectID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("FCM returned status %d", resp.StatusCode)
	}

	return nil
}

// IsInitialized returns true if Firebase is ready
func IsInitialized() bool {
	return client != nil
}