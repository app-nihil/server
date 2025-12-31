package websocket

import "time"

const (
	TypeAuth        = "auth"
	TypeMessageSend = "message.send"
	TypeMessageRead = "message.read"
	TypeTypingStart = "typing.start"
	TypeTypingStop  = "typing.stop"

	TypeAuthSuccess      = "auth.success"
	TypeAuthFailed       = "auth.failed"
	TypeMessageReceived  = "message.received"
	TypeMessageReadAck   = "message.read.ack"
	TypeTypingIndicator  = "typing.indicator"
	TypeChatExpired      = "chat.expired"
	TypeSubExpired       = "subscription.expired"
	TypeRateLimitWarning = "rate_limit.warning"
	TypeBanned           = "banned"
	TypeError            = "error"
)

type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type AuthPayload struct {
	DeviceUUID string `json:"device_uuid"`
	Signature  string `json:"signature"`
	Timestamp  int64  `json:"timestamp"`
}

type MessageSendPayload struct {
	ChatUUID         string `json:"chat_uuid"`
	MessageID        string `json:"message_id"`
	EncryptedContent string `json:"encrypted_content"`
}

type MessageReadPayload struct {
	ChatUUID  string `json:"chat_uuid"`
	MessageID string `json:"message_id"`
}

type TypingPayload struct {
	ChatUUID string `json:"chat_uuid"`
}

type AuthSuccessPayload struct {
	Chats        []ChatInfo       `json:"chats"`
	Subscription SubscriptionInfo `json:"subscription"`
}

type ChatInfo struct {
	ChatUUID    string    `json:"chat_uuid"`
	TTLSeconds  int       `json:"ttl_seconds"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	OtherDevice string    `json:"other_device,omitempty"`
}

type SubscriptionInfo struct {
	Plan      string    `json:"plan"`
	ExpiresAt time.Time `json:"expires_at"`
}

type AuthFailedPayload struct {
	Reason string `json:"reason"`
}

type MessageReceivedPayload struct {
	ChatUUID         string `json:"chat_uuid"`
	MessageID        string `json:"message_id"`
	SenderUUID       string `json:"sender_uuid"`
	EncryptedContent string `json:"encrypted_content"`
	Timestamp        int64  `json:"timestamp"`
}

type MessageReadAckPayload struct {
	ChatUUID  string `json:"chat_uuid"`
	MessageID string `json:"message_id"`
}

type ChatExpiredPayload struct {
	ChatUUID string `json:"chat_uuid"`
	Reason   string `json:"reason"`
}

type SubExpiredPayload struct {
	RenewURL string `json:"renew_url"`
}

type RateLimitWarningPayload struct {
	Current int `json:"current"`
	Limit   int `json:"limit"`
}

type BannedPayload struct {
	Reason string `json:"reason"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
