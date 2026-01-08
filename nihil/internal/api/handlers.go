package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	redisdb "nihil/internal/redis"
	stripeClient "nihil/internal/stripe"
	"nihil/internal/websocket"
)

type Handlers struct {
	redis *redisdb.Client
	hub   *websocket.Hub
}

func NewHandlers(redis *redisdb.Client, hub *websocket.Hub) *Handlers {
	return &Handlers{
		redis: redis,
		hub:   hub,
	}
}

func (h *Handlers) Health(c *gin.Context) {
	ctx := c.Request.Context()

	if err := h.redis.Ping(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "unhealthy",
			"error":  "redis unavailable",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"time":   time.Now().Unix(),
	})
}

type ValidateCodeRequest struct {
	Code string `json:"code" binding:"required"`
}

func (h *Handlers) ValidateActivationCode(c *gin.Context) {
	var req ValidateCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	ctx := c.Request.Context()
	code, err := h.redis.GetActivationCode(ctx, req.Code)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"valid": false,
			"error": "code not found",
		})
		return
	}

	if code.Status != "pending" {
		c.JSON(http.StatusBadRequest, gin.H{
			"valid": false,
			"error": "code already used",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"valid": true,
		"plan":  code.Plan,
		"type":  code.Type,
	})
}

type ClaimCodeRequest struct {
	Code       string `json:"code" binding:"required"`
	DeviceUUID string `json:"device_uuid" binding:"required"`
	PublicKey  string `json:"public_key" binding:"required"`
}

func (h *Handlers) ClaimActivationCode(c *gin.Context) {
	var req ClaimCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	ctx := c.Request.Context()
	sub, sessionID, err := h.redis.ClaimActivationCode(ctx, req.Code, req.DeviceUUID, req.PublicKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"session_id": sessionID,
		"subscription": gin.H{
			"plan":       sub.Plan,
			"plan_type":  sub.PlanType,
			"status":     sub.Status,
			"expires_at": sub.ExpiresAt.Unix(),
		},
	})
}

type RestoreSubscriptionRequest struct {
	SessionID  string `json:"session_id" binding:"required"`
	DeviceUUID string `json:"device_uuid" binding:"required"`
	PublicKey  string `json:"public_key" binding:"required"`
}

func (h *Handlers) RestoreSubscription(c *gin.Context) {
	var req RestoreSubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	ctx := c.Request.Context()

	existingSub, err := h.redis.GetSubscription(ctx, req.DeviceUUID)
	if err == nil && existingSub.Status == "active" && existingSub.ExpiresAt.After(time.Now()) {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"subscription": gin.H{
				"plan":       existingSub.Plan,
				"plan_type":  existingSub.PlanType,
				"status":     existingSub.Status,
				"expires_at": existingSub.ExpiresAt.Unix(),
			},
		})
		return
	}

	session, err := stripeClient.GetClient().GetCheckoutSession(req.SessionID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session"})
		return
	}

	if session.PaymentStatus != "paid" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payment not completed"})
		return
	}

	plan, ok := session.Metadata["plan"]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session metadata"})
		return
	}

	planType := "solo"
	if t, ok := session.Metadata["type"]; ok {
		planType = t
	}

	var duration time.Duration
	if planType == "team" {
		if d, ok := session.Metadata["duration"]; ok {
			duration = getTeamDurationFromMeta(d)
		} else {
			duration = 24 * time.Hour
		}
	} else {
		duration = getPlanDurationFromMeta(plan)
	}

	purchaseTime := time.Unix(session.Created, 0)
	expiresAt := purchaseTime.Add(duration)

	if time.Now().After(expiresAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subscription expired"})
		return
	}

	sub, err := h.redis.RestoreSubscription(ctx, req.DeviceUUID, req.PublicKey, plan, planType, expiresAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to restore subscription"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"subscription": gin.H{
			"plan":       sub.Plan,
			"plan_type":  sub.PlanType,
			"status":     sub.Status,
			"expires_at": sub.ExpiresAt.Unix(),
		},
	})
}

func getPlanDurationFromMeta(plan string) time.Duration {
	switch plan {
	case "1_day_solo", "1_day_duo":
		return 24 * time.Hour
	case "1_week_solo", "1_week_duo":
		return 7 * 24 * time.Hour
	case "1_month_solo", "1_month_duo":
		return 30 * 24 * time.Hour
	case "3_month_solo", "3_month_duo":
		return 90 * 24 * time.Hour
	case "1_year_solo", "1_year_duo":
		return 365 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

func getTeamDurationFromMeta(duration string) time.Duration {
	switch duration {
	case "1_day":
		return 24 * time.Hour
	case "1_week":
		return 7 * 24 * time.Hour
	case "1_month":
		return 30 * 24 * time.Hour
	case "3_month":
		return 90 * 24 * time.Hour
	case "1_year":
		return 365 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// ============================================
// CHAT ENDPOINTS (Anonymous Participant Model)
// ============================================

type CreateChatRequest struct {
	TTL               int    `json:"ttl" binding:"required"`
	ParticipantID     string `json:"participant_id" binding:"required"`
	ParticipantSecret string `json:"participant_secret" binding:"required"`
}

func (h *Handlers) CreateChat(c *gin.Context) {
	var req CreateChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	validTTLs := map[int]bool{5: true, 30: true, 60: true, 180: true, 300: true}
	if !validTTLs[req.TTL] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid TTL, must be 5, 30, 60, 180, or 300"})
		return
	}

	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	chatUUID := uuid.New().String()
	invitationToken, err := generateSecureToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	if err := h.redis.CreateChat(ctx, chatUUID, req.ParticipantID, req.ParticipantSecret, deviceUUID, invitationToken, req.TTL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create chat"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"chat_uuid":        chatUUID,
		"invitation_link":  "https://nihil.app/join/" + invitationToken,
		"invitation_token": invitationToken,
		"ttl":              req.TTL,
		"participant_id":   req.ParticipantID,
	})
}

type JoinChatRequest struct {
	InvitationToken   string `json:"invitation_token" binding:"required"`
	ParticipantID     string `json:"participant_id" binding:"required"`
	ParticipantSecret string `json:"participant_secret" binding:"required"`
}

func (h *Handlers) JoinChat(c *gin.Context) {
	var req JoinChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	joinerDeviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	// Pass joinerDeviceUUID so it gets stored in the chat
	chat, creatorDeviceUUID, err := h.redis.JoinChat(ctx, req.InvitationToken, joinerDeviceUUID, req.ParticipantID, req.ParticipantSecret)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if client, ok := h.hub.GetClient(creatorDeviceUUID); ok {
		client.SendMessage(&websocket.WSMessage{
			Type: "chat.joined",
			Payload: gin.H{
				"chat_uuid":          chat.ChatUUID,
				"participant_id":     req.ParticipantID,
				"joiner_device_uuid": joinerDeviceUUID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"chat_uuid":         chat.ChatUUID,
		"ttl":               chat.TTLSeconds,
		"other_device_uuid": creatorDeviceUUID,
		"participant_id":    req.ParticipantID,
	})
}

func (h *Handlers) ListChats(c *gin.Context) {
	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	chatUUIDs, err := h.redis.GetUserChats(ctx, deviceUUID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get chats"})
		return
	}

	chats := make([]gin.H, 0, len(chatUUIDs))
	for _, chatUUID := range chatUUIDs {
		chat, err := h.redis.GetChat(ctx, chatUUID)
		if err != nil {
			continue
		}

		otherDevice := ""
		if chat.ParticipantA == deviceUUID {
			otherDevice = chat.ParticipantB
		} else {
			otherDevice = chat.ParticipantA
		}

		chats = append(chats, gin.H{
			"chat_uuid":    chat.ChatUUID,
			"ttl_seconds":  chat.TTLSeconds,
			"status":       chat.Status,
			"created_at":   chat.CreatedAt,
			"other_device": otherDevice,
		})
	}

	c.JSON(http.StatusOK, gin.H{"chats": chats})
}

type DeleteChatRequest struct {
	ParticipantID     string `json:"participant_id" binding:"required"`
	ParticipantSecret string `json:"participant_secret" binding:"required"`
}

func (h *Handlers) DeleteChat(c *gin.Context) {
	chatUUID := c.Param("chat_uuid")
	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	// Parse request body for participant credentials (backward compatibility)
	var req DeleteChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request - participant credentials required"})
		return
	}

	// Get chat first (needed for BroadcastToChat before deletion)
	chat, err := h.redis.GetChat(ctx, chatUUID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
		return
	}

	// First try to validate by device UUID (more reliable)
	isParticipant, _, err := h.redis.IsDeviceParticipant(ctx, chatUUID, deviceUUID)
	if err != nil || !isParticipant {
		// Fallback to credential validation (for backward compatibility)
		valid, err := h.redis.ValidateParticipant(ctx, chatUUID, req.ParticipantID, req.ParticipantSecret)
		if err != nil || !valid {
			c.JSON(http.StatusForbidden, gin.H{"error": "not a participant"})
			return
		}
	}

	// Delete push registrations first
	h.redis.DeleteAllPushForChat(ctx, chatUUID)

	// Notify BOTH participants BEFORE deleting the chat using device UUIDs
	expiredMsg := &websocket.WSMessage{
		Type: "chat.expired",
		Payload: gin.H{
			"chat_uuid": chatUUID,
			"reason":    "deleted_by_participant",
		},
	}

	// Send to participant A if connected
	if chat.ParticipantADevice != "" {
		if client, ok := h.hub.GetClient(chat.ParticipantADevice); ok {
			client.SendMessage(expiredMsg)
		}
	}

	// Send to participant B if connected
	if chat.ParticipantBDevice != "" {
		if client, ok := h.hub.GetClient(chat.ParticipantBDevice); ok {
			client.SendMessage(expiredMsg)
		}
	}

	// Now delete the chat from Redis
	if err := h.redis.DeleteChat(ctx, chatUUID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete chat"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handlers) GetSubscriptionStatus(c *gin.Context) {
	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	sub, err := h.redis.GetSubscription(ctx, deviceUUID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"plan":       sub.Plan,
		"plan_type":  sub.PlanType,
		"status":     sub.Status,
		"expires_at": sub.ExpiresAt.Unix(),
	})
}

// ============================================
// CHECKOUT ENDPOINTS
// ============================================

type CreateCheckoutRequest struct {
	Plan string `json:"plan" binding:"required"`
}

func (h *Handlers) CreateCheckout(c *gin.Context) {
	var req CreateCheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	if !stripeClient.IsPlanValid(req.Plan) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan"})
		return
	}

	successURL := "https://nihil.app/activate?session_id={CHECKOUT_SESSION_ID}"
	cancelURL := "https://nihil.app/#pricing"

	sess, err := stripeClient.GetClient().CreateCheckoutSession(req.Plan, successURL, cancelURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create checkout session"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"checkout_url": sess.URL,
		"session_id":   sess.ID,
	})
}

type CreateTeamCheckoutRequest struct {
	Duration    string `json:"duration" binding:"required"`
	DeviceCount int    `json:"device_count" binding:"required"`
}

func (h *Handlers) CreateTeamCheckout(c *gin.Context) {
	var req CreateTeamCheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	if !stripeClient.IsTeamDurationValid(req.Duration) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid duration"})
		return
	}

	if req.DeviceCount < 3 || req.DeviceCount > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device count must be between 3 and 50"})
		return
	}

	successURL := "https://nihil.app/activate?session_id={CHECKOUT_SESSION_ID}"
	cancelURL := "https://nihil.app/#pricing"

	sess, err := stripeClient.GetClient().CreateTeamCheckoutSession(req.Duration, req.DeviceCount, successURL, cancelURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create checkout session"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"checkout_url": sess.URL,
		"session_id":   sess.ID,
	})
}

func (h *Handlers) CalculateTeamPrice(c *gin.Context) {
	duration := c.Query("duration")
	deviceCountStr := c.Query("device_count")

	if duration == "" || deviceCountStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "duration and device_count required"})
		return
	}

	deviceCount, err := strconv.Atoi(deviceCountStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid device_count"})
		return
	}

	pricePerDevice, totalPrice, discountPercent, err := stripeClient.CalculateTeamPrice(duration, deviceCount)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"price_per_device": pricePerDevice,
		"total_price":      totalPrice,
		"discount_percent": discountPercent,
		"device_count":     deviceCount,
		"duration":         duration,
	})
}

func generateSecureToken() (string, error) {
	bytes := make([]byte, 20)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	h := hex.EncodeToString(bytes)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20], nil
}

func (h *Handlers) GetActivationCodes(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id required"})
		return
	}

	ctx := c.Request.Context()
	codes, err := h.redis.GetActivationCodesBySession(ctx, sessionID)
	if err != nil || len(codes) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "codes not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"codes": codes})
}

// ============================================
// KEY EXCHANGE ENDPOINTS (Signal Protocol)
// ============================================

type RegisterKeysRequest struct {
	RegistrationID int              `json:"registration_id" binding:"required"`
	IdentityKey    string           `json:"identity_key" binding:"required"`
	SignedPreKey   SignedPreKeyData `json:"signed_prekey" binding:"required"`
	PreKeys        []PreKeyData     `json:"prekeys" binding:"required"`
}

type SignedPreKeyData struct {
	ID        int    `json:"id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

type PreKeyData struct {
	ID        int    `json:"id"`
	PublicKey string `json:"public_key"`
}

func (h *Handlers) RegisterKeys(c *gin.Context) {
	var req RegisterKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	// Convert to redis types
	signedPreKey := redisdb.SignedPreKey{
		ID:        req.SignedPreKey.ID,
		PublicKey: req.SignedPreKey.PublicKey,
		Signature: req.SignedPreKey.Signature,
	}

	preKeys := make([]redisdb.PreKey, len(req.PreKeys))
	for i, pk := range req.PreKeys {
		preKeys[i] = redisdb.PreKey{
			ID:        pk.ID,
			PublicKey: pk.PublicKey,
		}
	}

	if err := h.redis.StoreKeyBundle(ctx, deviceUUID, req.RegistrationID, req.IdentityKey, signedPreKey, preKeys); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// RegisterKeysPublicRequest includes device_uuid since auth headers aren't available yet
type RegisterKeysPublicRequest struct {
	DeviceUUID     string           `json:"device_uuid" binding:"required"`
	RegistrationID int              `json:"registration_id" binding:"required"`
	IdentityKey    string           `json:"identity_key" binding:"required"`
	SignedPreKey   SignedPreKeyData `json:"signed_prekey" binding:"required"`
	PreKeys        []PreKeyData     `json:"prekeys" binding:"required"`
}

// RegisterKeysPublic - public endpoint for key registration (called right after activation)
func (h *Handlers) RegisterKeysPublic(c *gin.Context) {
	var req RegisterKeysPublicRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	ctx := c.Request.Context()

	// Verify device has an active subscription (prevents abuse)
	active, _ := h.redis.IsSubscriptionActive(ctx, req.DeviceUUID)
	if !active {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no active subscription"})
		return
	}

	// Convert to redis types
	signedPreKey := redisdb.SignedPreKey{
		ID:        req.SignedPreKey.ID,
		PublicKey: req.SignedPreKey.PublicKey,
		Signature: req.SignedPreKey.Signature,
	}

	preKeys := make([]redisdb.PreKey, len(req.PreKeys))
	for i, pk := range req.PreKeys {
		preKeys[i] = redisdb.PreKey{
			ID:        pk.ID,
			PublicKey: pk.PublicKey,
		}
	}

	if err := h.redis.StoreKeyBundle(ctx, req.DeviceUUID, req.RegistrationID, req.IdentityKey, signedPreKey, preKeys); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handlers) GetKeyBundle(c *gin.Context) {
	targetUUID := c.Param("device_uuid")
	ctx := c.Request.Context()

	// GetKeyBundle now includes consuming one prekey atomically
	bundle, err := h.redis.GetKeyBundle(ctx, targetUUID)
	if err != nil || bundle == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "key bundle not found"})
		return
	}

	response := gin.H{
		"registration_id": bundle.RegistrationID,
		"identity_key":    bundle.IdentityKey,
		"signed_prekey": gin.H{
			"id":         bundle.SignedPreKey.ID,
			"public_key": bundle.SignedPreKey.PublicKey,
			"signature":  bundle.SignedPreKey.Signature,
		},
	}

	// PreKey is already consumed and included in bundle by GetKeyBundle
	if bundle.PreKey != nil {
		response["prekey"] = gin.H{
			"id":         bundle.PreKey.ID,
			"public_key": bundle.PreKey.PublicKey,
		}
	}

	c.JSON(http.StatusOK, response)
}

type ReplenishKeysRequest struct {
	PreKeys []PreKeyData `json:"prekeys" binding:"required"`
}

func (h *Handlers) ReplenishKeys(c *gin.Context) {
	var req ReplenishKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	preKeys := make([]redisdb.PreKey, len(req.PreKeys))
	for i, pk := range req.PreKeys {
		preKeys[i] = redisdb.PreKey{
			ID:        pk.ID,
			PublicKey: pk.PublicKey,
		}
	}

	if err := h.redis.AddPreKeys(ctx, deviceUUID, preKeys); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add prekeys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handlers) GetPreKeyCount(c *gin.Context) {
	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	count, err := h.redis.GetPreKeyCount(ctx, deviceUUID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get prekey count"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"count": count})
}

// ============================================
// PUSH NOTIFICATIONS - DEPRECATED
// ============================================

func (h *Handlers) RegisterFCMToken(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"deprecated": true,
		"message":    "Push is now chat-scoped. Use WebSocket push.register message instead.",
	})
}

func (h *Handlers) PurgeDevice(c *gin.Context) {
	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	if err := h.redis.PurgeDevice(ctx, deviceUUID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to purge device"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}