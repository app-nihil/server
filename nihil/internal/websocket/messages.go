package websocket

import "time"

// Message types
const (
	TypeAuth             = "auth"
	TypeAuthSuccess      = "auth.success"
	TypeAuthFailed       = "auth.failed"
	TypeChatRegister     = "chat.register"
	TypeChatRegisterAck  = "chat.register.ack"
	TypeChatJoined       = "chat.joined"
	TypeMessageSend      = "message.send"
	TypeMessageReceived  = "message.received"
	TypeMessageRead      = "message.read"
	TypeMessageReadAck   = "message.read.ack"
	TypeTypingStart      = "typing.start"
	TypeTypingStop       = "typing.stop"
	TypeTypingIndicator  = "typing.indicator"
	TypeChatExpired      = "chat.expired"
	TypeSubExpired       = "subscription.expired"
	TypeRateLimitWarning = "rate_limit.warning"
	TypeBanned           = "banned"
	TypeError            = "error"
	TypePushRegister     = "push.register"
	TypePushRegisterAck  = "push.register.ack"
	TypePushUnregister   = "push.unregister"
	TypePushUnregisterAck = "push.unregister.ack"
	TypePushBurnAll      = "push.burn_all"
	TypePushBurnAllAck   = "push.burn_all.ack"
)

type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

type AuthPayload struct {
	DeviceUUID string `json:"device_uuid"`
	Signature  string `json:"signature"`
	Timestamp  int64  `json:"timestamp"`
}

type AuthSuccessPayload struct {
	Chats        []ChatInfo       `json:"chats"`
	Subscription SubscriptionInfo `json:"subscription"`
}

type AuthFailedPayload struct {
	Reason string `json:"reason"`
}

type ChatInfo struct {
	ChatUUID  string `json:"chat_uuid"`
	CreatedAt int64  `json:"created_at"`
	TTL       int    `json:"ttl"`
	Status    string `json:"status"`
}

type SubscriptionInfo struct {
	Plan      string    `json:"plan"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ChatRegisterPayload - client sends their locally-stored chats with credentials
type ChatRegisterPayload struct {
	Chats []ChatRegistration `json:"chats"`
}

type ChatRegistration struct {
	ChatUUID          string `json:"chat_uuid"`
	ParticipantID     string `json:"participant_id"`
	ParticipantSecret string `json:"participant_secret"`
}

type ChatRegisterAckPayload struct {
	Registered int `json:"registered"`
	Failed     int `json:"failed"`
}

// ChatJoinedPayload - sent to chat creator when someone joins
type ChatJoinedPayload struct {
	ChatUUID         string `json:"chat_uuid"`
	JoinerDeviceUUID string `json:"joiner_device_uuid"`
	ParticipantID    string `json:"participant_id"`
}

type MessageSendPayload struct {
	ChatUUID          string `json:"chat_uuid"`
	MessageID         string `json:"message_id"`
	EncryptedContent  string `json:"encrypted_content"`
	ParticipantID     string `json:"participant_id"`
	ParticipantSecret string `json:"participant_secret"`
}

type MessageReceivedPayload struct {
	ChatUUID         string `json:"chat_uuid"`
	MessageID        string `json:"message_id"`
	SenderUUID       string `json:"sender_uuid"`        // Participant ID (for routing)
	SenderDeviceUUID string `json:"sender_device_uuid"` // Device UUID (for Signal decryption)
	EncryptedContent string `json:"encrypted_content"`
	Timestamp        int64  `json:"timestamp"`
}

type MessageReadPayload struct {
	ChatUUID  string `json:"chat_uuid"`
	MessageID string `json:"message_id"`
}

type MessageReadAckPayload struct {
	ChatUUID  string `json:"chat_uuid"`
	MessageID string `json:"message_id"`
}

type TypingPayload struct {
	ChatUUID          string `json:"chat_uuid"`
	ParticipantID     string `json:"participant_id,omitempty"`
	ParticipantSecret string `json:"participant_secret,omitempty"`
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

// Push notification payloads
type PushRegisterPayload struct {
	ChatUUID          string `json:"chat_uuid"`
	FCMToken          string `json:"fcm_token"`
	ParticipantID     string `json:"participant_id"`
	ParticipantSecret string `json:"participant_secret"`
}

type PushRegisterAckPayload struct {
	ChatUUID string `json:"chat_uuid"`
	Success  bool   `json:"success"`
}

type PushUnregisterPayload struct {
	ChatUUID          string `json:"chat_uuid"`
	ParticipantID     string `json:"participant_id"`
	ParticipantSecret string `json:"participant_secret"`
}

type PushUnregisterAckPayload struct {
	ChatUUID string `json:"chat_uuid"`
	Success  bool   `json:"success"`
}

type PushBurnAllPayload struct {
	ParticipantIDs []string `json:"participant_ids"`
}

type PushBurnAllAckPayload struct {
	Deleted int `json:"deleted"`
}