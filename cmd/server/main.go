package main

import (
	"log"
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

	log.Println("Connecting to Redis...")
	redis, err := redisdb.NewClient(cfg.RedisURL)
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer redis.Close()
	log.Println("Connected to Redis")

	// Initialize Firebase for push notifications
	firebaseKeyPath := "/opt/nihil/firebase-key.json"
	firebaseProject := "nihil-3176a"
	
	if firebaseJSON, err := os.ReadFile(firebaseKeyPath); err == nil {
		if err := firebase.Initialize(firebaseProject, firebaseJSON); err != nil {
			log.Printf("Warning: Firebase init failed: %v", err)
		} else {
			log.Println("Firebase initialized for push notifications")
		}
	} else {
		log.Printf("Warning: Firebase key not found at %s (push notifications disabled)", firebaseKeyPath)
	}

	hub := websocket.NewHub(redis, cfg.RateLimitPerMinute)
	go hub.Run()
	log.Println("WebSocket hub started")

	if cfg.StripeSecretKey != "" {
		stripeClient.NewClient(cfg.StripeSecretKey)
		log.Println("Stripe client initialized")
	} else {
		log.Println("Warning: Stripe not configured")
	}

	router := gin.New()
	api.SetupRoutes(router, redis, hub, cfg.CORSOrigins, cfg.RateLimitPerMinute)

	if cfg.StripeWebhookSecret != "" {
		webhookHandler := stripeClient.NewWebhookHandler(redis, cfg.StripeWebhookSecret)
		webhookHandler.RegisterRoutes(router)
		log.Println("Stripe webhook handler registered")
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("Shutting down server...")
		redis.Close()
		os.Exit(0)
	}()

	addr := ":" + cfg.Port
	log.Printf("Server starting on %s", addr)
	log.Printf("Environment: %s", cfg.Environment)

	if err := router.Run(addr); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
