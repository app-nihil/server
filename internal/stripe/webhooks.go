package stripe

import (
"context"
"crypto/rand"
"encoding/hex"
"encoding/json"
"fmt"
"io"
"log"
"net/http"
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
log.Printf("Webhook signature verification failed: %v", err)
c.JSON(http.StatusBadRequest, gin.H{"error": "invalid signature"})
return
}

ctx := context.Background()

switch event.Type {
case "checkout.session.completed":
h.handleCheckoutCompleted(ctx, event)
case "customer.subscription.deleted":
h.handleSubscriptionDeleted(ctx, event)
default:
log.Printf("Unhandled event type: %s", event.Type)
}

c.JSON(http.StatusOK, gin.H{"received": true})
}

func (h *WebhookHandler) handleCheckoutCompleted(ctx context.Context, event stripe.Event) {
var session stripe.CheckoutSession
if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
log.Printf("Failed to parse checkout session: %v", err)
return
}

plan := session.Metadata["plan"]
planType := session.Metadata["type"]

if planType == "duo" {
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
if err := h.redis.CreateActivationCode(ctx, ownerAC); err != nil {
log.Printf("Failed to create owner activation code: %v", err)
return
}

guestAC := &redisdb.ActivationCode{
Code:            guestCode,
StripeSessionID: session.ID,
Plan:            plan,
Type:            "duo_guest",
Status:          "pending",
CreatedAt:       time.Now(),
DuoOwnerCode:    ownerCode,
}
if err := h.redis.CreateActivationCode(ctx, guestAC); err != nil {
log.Printf("Failed to create guest activation code: %v", err)
return
}

log.Printf("DUO codes generated - Owner: %s, Guest: %s", ownerCode, guestCode)
} else {
code := generateActivationCode()

ac := &redisdb.ActivationCode{
Code:            code,
StripeSessionID: session.ID,
Plan:            plan,
Type:            "solo",
Status:          "pending",
CreatedAt:       time.Now(),
}
if err := h.redis.CreateActivationCode(ctx, ac); err != nil {
log.Printf("Failed to create activation code: %v", err)
return
}

log.Printf("SOLO code generated: %s", code)
}
}

func (h *WebhookHandler) handleSubscriptionDeleted(ctx context.Context, event stripe.Event) {
log.Printf("Subscription deleted event received")
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
