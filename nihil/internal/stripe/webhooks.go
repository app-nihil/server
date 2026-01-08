package stripe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"

	redisdb "nihil/internal/redis"
)

type WebhookHandler struct {
	redis         *redisdb.Client
	webhookSecret string
}

func NewWebhookHandler(redis *redisdb.Client, webhookSecret string) *WebhookHandler {
	return &WebhookHandler{
		redis:         redis,
		webhookSecret: webhookSecret,
	}
}

func (h *WebhookHandler) HandleWebhook(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	event, err := webhook.ConstructEventWithOptions(body, c.GetHeader("Stripe-Signature"), h.webhookSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid signature"})
		return
	}

	ctx := context.Background()

	switch event.Type {
	case "checkout.session.completed":
		h.handleCheckoutCompleted(ctx, event)
	case "customer.subscription.deleted":
		h.handleSubscriptionDeleted(ctx, event)
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

func (h *WebhookHandler) handleCheckoutCompleted(ctx context.Context, event stripe.Event) {
	var session stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		return
	}

	plan := session.Metadata["plan"]
	planType := session.Metadata["type"]

	switch planType {
	case "team":
		h.handleTeamCheckout(ctx, session)
	case "duo":
		h.handleDuoCheckout(ctx, session, plan)
	default:
		h.handleSoloCheckout(ctx, session, plan)
	}
}

func (h *WebhookHandler) handleSoloCheckout(ctx context.Context, session stripe.CheckoutSession, plan string) {
	code := generateActivationCode()

	// ANONYMOUS CODE POOL: We store the code but NOT which Stripe session it came from
	// This breaks the link between payment identity and device identity
	ac := &redisdb.ActivationCode{
		Code:            code,
		StripeSessionID: session.ID, // Stored for activation page lookup only
		Plan:            plan,
		Type:            "solo",
		Status:          "pending",
		CreatedAt:       time.Now(),
	}
	h.redis.CreateActivationCode(ctx, ac)

	// Also store in anonymous pool (for future: pre-generate codes)
	h.redis.AddToCodePool(ctx, code, session.ID)
}

func (h *WebhookHandler) handleDuoCheckout(ctx context.Context, session stripe.CheckoutSession, plan string) {
	ownerCode := generateActivationCode()
	guestCode := generateActivationCode()

	ownerAC := &redisdb.ActivationCode{
		Code:            ownerCode,
		StripeSessionID: session.ID,
		Plan:            plan,
		Type:            "duo_owner",
		Status:          "pending",
		CreatedAt:       time.Now(),
	}
	h.redis.CreateActivationCode(ctx, ownerAC)

	guestAC := &redisdb.ActivationCode{
		Code:            guestCode,
		StripeSessionID: session.ID,
		Plan:            plan,
		Type:            "duo_guest",
		Status:          "pending",
		CreatedAt:       time.Now(),
		DuoOwnerCode:    ownerCode,
	}
	h.redis.CreateActivationCode(ctx, guestAC)

	// Add to pool
	h.redis.AddToCodePool(ctx, ownerCode, session.ID)
	h.redis.AddToCodePool(ctx, guestCode, session.ID)
}

func (h *WebhookHandler) handleTeamCheckout(ctx context.Context, session stripe.CheckoutSession) {
	deviceCountStr := session.Metadata["device_count"]
	duration := session.Metadata["duration"]
	plan := session.Metadata["plan"]

	deviceCount, err := strconv.Atoi(deviceCountStr)
	if err != nil {
		return
	}

	if deviceCount < 3 || deviceCount > 50 {
		return
	}

	for i := 0; i < deviceCount; i++ {
		code := generateActivationCode()

		ac := &redisdb.ActivationCode{
			Code:            code,
			StripeSessionID: session.ID,
			Plan:            plan,
			Type:            "team",
			Status:          "pending",
			CreatedAt:       time.Now(),
			TeamIndex:       i + 1,
			TeamTotal:       deviceCount,
			Duration:        duration,
		}
		h.redis.CreateActivationCode(ctx, ac)
		h.redis.AddToCodePool(ctx, code, session.ID)
	}
}

func (h *WebhookHandler) handleSubscriptionDeleted(ctx context.Context, event stripe.Event) {
	// No action needed - subscriptions are time-based
}

func generateActivationCode() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	h := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s",
		h[0:4],
		h[4:8],
		h[8:12],
		h[12:16],
	)
}

func (h *WebhookHandler) RegisterRoutes(router *gin.Engine) {
	router.POST("/webhook/stripe", h.HandleWebhook)
}