package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"nihil/internal/api"
	"nihil/internal/config"
	"nihil/internal/firebase"
	redisdb "nihil/internal/redis"
	stripeClient "nihil/internal/stripe"
	"nihil/internal/websocket"
)

func main() {
	godotenv.Load()

	cfg := config.Load()

	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	redis, err := redisdb.NewClient(cfg.RedisURL)
	if err != nil {
		os.Exit(1)
	}
	defer redis.Close()

	firebaseKeyPath := "/opt/nihil/firebase-key.json"
	firebaseProject := "nihil-3176a"

	if firebaseJSON, err := os.ReadFile(firebaseKeyPath); err == nil {
		firebase.Initialize(firebaseProject, firebaseJSON)
	}

	hub := websocket.NewHub(redis, cfg.RateLimitPerMinute)
	go hub.Run()

	if cfg.StripeSecretKey != "" {
		stripeClient.NewClient(cfg.StripeSecretKey)
	}

	router := gin.New()
	api.SetupRoutes(router, redis, hub, cfg.CORSOrigins, cfg.RateLimitPerMinute)

	if cfg.StripeWebhookSecret != "" {
		webhookHandler := stripeClient.NewWebhookHandler(redis, cfg.StripeWebhookSecret)
		webhookHandler.RegisterRoutes(router)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		redis.Close()
		os.Exit(0)
	}()

	addr := ":" + cfg.Port
	router.Run(addr)
}