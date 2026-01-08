package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	redisdb "nihil/internal/redis"
	ws "nihil/internal/websocket"
)

func SetupRoutes(router *gin.Engine, redis *redisdb.Client, hub *ws.Hub, corsOrigins string, rateLimit int) {
	handlers := NewHandlers(redis, hub)
	middleware := NewMiddleware(redis)

	// Create upgrader with origin check
	allowedOrigins := strings.Split(corsOrigins, ",")
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			
			// Allow configured origins
			for _, o := range allowedOrigins {
				if strings.TrimSpace(o) == origin {
					return true
				}
			}
			
			// Allow localhost for development
			if strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
				return true
			}
			
			return false
		},
	}

	router.Use(CORS(corsOrigins))
	router.Use(RequestLogger())
	router.Use(gin.Recovery())

	// Public endpoints
	router.GET("/health", handlers.Health)
	router.POST("/activation/validate", handlers.ValidateActivationCode)
	router.POST("/activation/claim", handlers.ClaimActivationCode)
	router.POST("/checkout/create", handlers.CreateCheckout)
	router.POST("/checkout/team", handlers.CreateTeamCheckout)
	router.GET("/checkout/team/calculate", handlers.CalculateTeamPrice)
	router.GET("/activation/codes", handlers.GetActivationCodes)

	// Subscription restoration (public - verifies with Stripe)
	router.POST("/subscription/restore", handlers.RestoreSubscription)

	// Key registration (public - called right after activation, before auth is possible)
	router.POST("/keys/register", handlers.RegisterKeysPublic)

	// WebSocket
	router.GET("/ws", func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		client := ws.NewClient(hub, conn)
		hub.Register(client)
		go client.WritePump()
		go client.ReadPump()
	})

	// Authenticated endpoints
	auth := router.Group("/")
	auth.Use(middleware.DeviceAuth())
	auth.Use(middleware.RateLimit(rateLimit))
	{
		// Chat management
		auth.POST("/chat/create", handlers.CreateChat)
		auth.POST("/chat/join", handlers.JoinChat)
		auth.GET("/chat/list", handlers.ListChats)
		auth.DELETE("/chat/:chat_uuid", handlers.DeleteChat)

		// Subscription
		auth.GET("/subscription/status", handlers.GetSubscriptionStatus)

		// Key exchange (Signal Protocol)
		auth.GET("/keys/:device_uuid", handlers.GetKeyBundle)
		auth.POST("/keys/replenish", handlers.ReplenishKeys)
		auth.GET("/keys/count", handlers.GetPreKeyCount)

		// Push notifications
		auth.POST("/device/fcm-token", handlers.RegisterFCMToken)
		auth.DELETE("/device/purge", handlers.PurgeDevice)
	}
}