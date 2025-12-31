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
		"session_id": sessionID, // App stores this for restoration
		"subscription": gin.H{
			"plan":       sub.Plan,
			"plan_type":  sub.PlanType,
			"status":     sub.Status,
			"expires_at": sub.ExpiresAt.Unix(),
		},
	})
}

// RestoreSubscription allows app to restore subscription after server restart
// App sends session_id, server verifies with Stripe and restores subscription
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

	// Check if subscription already exists in cache
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

	// Verify with Stripe
	session, err := stripeClient.GetClient().GetCheckoutSession(req.SessionID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session"})
		return
	}

	// Check payment status
	if session.PaymentStatus != "paid" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payment not completed"})
		return
	}

	// Get plan from metadata
	plan, ok := session.Metadata["plan"]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid session metadata"})
		return
	}

	// Calculate expiry from plan and purchase time
	planType := "solo"
	if t, ok := session.Metadata["type"]; ok {
		planType = t
	}

	var duration time.Duration
	if planType == "team" {
		if d, ok := session.Metadata["duration"]; ok {
			duration = getTeamDurationFromMeta(d)
		} else {
			duration = 24 * time.Hour // fallback
		}
	} else {
		duration = getPlanDurationFromMeta(plan)
	}

	// Calculate expiry from when payment was made
	purchaseTime := time.Unix(session.Created, 0)
	expiresAt := purchaseTime.Add(duration)

	// Check if subscription has expired
	if time.Now().After(expiresAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subscription expired"})
		return
	}

	// Restore the subscription
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

type CreateChatRequest struct {
	TTL int `json:"ttl" binding:"required"`
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

	if err := h.redis.CreateChat(ctx, chatUUID, deviceUUID, invitationToken, req.TTL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create chat"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"chat_uuid":        chatUUID,
		"invitation_link":  "https://nihil.app/join/" + invitationToken,
		"invitation_token": invitationToken,
		"ttl":              req.TTL,
	})
}

type JoinChatRequest struct {
	InvitationToken string `json:"invitation_token" binding:"required"`
}

func (h *Handlers) JoinChat(c *gin.Context) {
	var req JoinChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	chat, err := h.redis.JoinChat(ctx, req.InvitationToken, deviceUUID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if client, ok := h.hub.GetClient(chat.ParticipantA); ok {
		client.SendMessage(&websocket.WSMessage{
			Type: "chat.joined",
			Payload: gin.H{
				"chat_uuid": chat.ChatUUID,
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"chat_uuid":     chat.ChatUUID,
		"participant_a": chat.ParticipantA,
		"ttl":           chat.TTLSeconds,
		"status":        chat.Status,
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
			"ttl":          chat.TTLSeconds,
			"status":       chat.Status,
			"created_at":   chat.CreatedAt.Unix(),
			"other_device": otherDevice,
		})
	}

	c.JSON(http.StatusOK, gin.H{"chats": chats})
}

func (h *Handlers) DeleteChat(c *gin.Context) {
	chatUUID := c.Param("chat_uuid")
	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	chat, err := h.redis.GetChat(ctx, chatUUID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
		return
	}

	if chat.ParticipantA != deviceUUID && chat.ParticipantB != deviceUUID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not a participant"})
		return
	}

	h.hub.BroadcastToChat(ctx, chatUUID, &websocket.WSMessage{
		Type: websocket.TypeChatExpired,
		Payload: websocket.ChatExpiredPayload{
			ChatUUID: chatUUID,
			Reason:   "deleted",
		},
	})

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

// CreateTeamCheckout handles TEAM plan checkout with dynamic pricing
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

// CalculateTeamPrice endpoint for live price calculation on frontend
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

// generateSecureToken creates a cryptographically secure token
// Format: xxxxxxxx-xxxx-xxxx-xxxx (20 bytes = 160 bits of entropy)
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

	keyBundle := &redisdb.KeyBundle{
		DeviceUUID:     deviceUUID,
		RegistrationID: req.RegistrationID,
		IdentityKey:    req.IdentityKey,
		SignedPreKey: redisdb.SignedPreKey{
			ID:        req.SignedPreKey.ID,
			PublicKey: req.SignedPreKey.PublicKey,
			Signature: req.SignedPreKey.Signature,
		},
		PreKeys: make([]redisdb.PreKey, len(req.PreKeys)),
	}

	for i, pk := range req.PreKeys {
		keyBundle.PreKeys[i] = redisdb.PreKey{
			ID:        pk.ID,
			PublicKey: pk.PublicKey,
		}
	}

	if err := h.redis.StoreKeyBundle(ctx, keyBundle); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store keys"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handlers) GetKeyBundle(c *gin.Context) {
	targetUUID := c.Param("device_uuid")
	ctx := c.Request.Context()

	bundle, err := h.redis.GetKeyBundle(ctx, targetUUID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "key bundle not found"})
		return
	}

	// Get and consume one prekey
	preKey, err := h.redis.ConsumePreKey(ctx, targetUUID)

	response := gin.H{
		"registration_id": bundle.RegistrationID,
		"identity_key":    bundle.IdentityKey,
		"signed_prekey": gin.H{
			"id":         bundle.SignedPreKey.ID,
			"public_key": bundle.SignedPreKey.PublicKey,
			"signature":  bundle.SignedPreKey.Signature,
		},
	}

	if err == nil && preKey != nil {
		response["prekey"] = gin.H{
			"id":         preKey.ID,
			"public_key": preKey.PublicKey,
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
// PUSH NOTIFICATIONS
// ============================================

type RegisterFCMTokenRequest struct {
	FCMToken string `json:"fcm_token" binding:"required"`
}

func (h *Handlers) RegisterFCMToken(c *gin.Context) {
	var req RegisterFCMTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	deviceUUID := c.GetString("device_uuid")
	ctx := c.Request.Context()

	if err := h.redis.StoreFCMToken(ctx, deviceUUID, req.FCMToken); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
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