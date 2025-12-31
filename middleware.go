package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	redisdb "nihil/internal/redis"
)

type Middleware struct {
	redis *redisdb.Client
}

func NewMiddleware(redis *redisdb.Client) *Middleware {
	return &Middleware{redis: redis}
}

func (m *Middleware) DeviceAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceUUID := c.GetHeader("X-Device-UUID")
		timestampStr := c.GetHeader("X-Timestamp")
		signature := c.GetHeader("X-Signature")

		if deviceUUID == "" || timestampStr == "" || signature == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing authentication headers",
			})
			return
		}

		timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid timestamp",
			})
			return
		}

		now := time.Now().Unix()
		if abs(now-timestamp) > 300 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "timestamp expired",
			})
			return
		}

		ctx := c.Request.Context()

		banned, reason, _ := m.redis.IsBanned(ctx, deviceUUID)
		if banned {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":  "device banned",
				"reason": reason,
			})
			return
		}

		publicKey, err := m.redis.GetDevicePublicKey(ctx, deviceUUID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "device not found",
			})
			return
		}

		expectedSig := computeSignature(publicKey, deviceUUID, timestamp)
		if signature != expectedSig {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid signature",
			})
			return
		}

		active, _ := m.redis.IsSubscriptionActive(ctx, deviceUUID)
		if !active {
			c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
				"error":     "subscription expired",
				"renew_url": "https://nihil.app",
			})
			return
		}

		c.Set("device_uuid", deviceUUID)
		c.Next()
	}
}

func (m *Middleware) RateLimit(limit int) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceUUID := c.GetString("device_uuid")
		if deviceUUID == "" {
			c.Next()
			return
		}

		ctx := c.Request.Context()

		count, allowed, err := m.redis.CheckRateLimit(ctx, deviceUUID, limit)
		if err != nil {
			c.Next()
			return
		}

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":   "rate limit exceeded",
				"current": count,
				"limit":   limit,
			})
			return
		}

		c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", limit))
		c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", limit-count))

		c.Next()
	}
}

func CORS(origins string) gin.HandlerFunc {
	allowedOrigins := strings.Split(origins, ",")

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		allowed := false
		for _, o := range allowedOrigins {
			if strings.TrimSpace(o) == origin {
				allowed = true
				break
			}
		}

		if strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
			allowed = true
		}

		if allowed {
			c.Header("Access-Control-Allow-Origin", origin)
		}

		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, X-Device-UUID, X-Timestamp, X-Signature")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// RequestLogger returns a no-op middleware - we don't log requests
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
	}
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func computeSignature(key, deviceUUID string, timestamp int64) string {
	data := fmt.Sprintf("%s:%d", deviceUUID, timestamp)
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}