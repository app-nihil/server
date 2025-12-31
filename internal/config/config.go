package config

import (
"os"
"strconv"
)

type Config struct {
Port                 string
RedisURL             string
StripeSecretKey      string
StripeWebhookSecret  string
CORSOrigins          string
Environment          string
RateLimitPerMinute   int
MessageMaxSize       int
}

func Load() *Config {
return &Config{
Port:                 getEnv("PORT", "8080"),
RedisURL:             getEnv("REDIS_URL", "redis://localhost:6379"),
StripeSecretKey:      getEnv("STRIPE_SECRET_KEY", ""),
StripeWebhookSecret:  getEnv("STRIPE_WEBHOOK_SECRET", ""),
CORSOrigins:          getEnv("CORS_ORIGINS", "https://nihil.app"),
Environment:          getEnv("ENVIRONMENT", "development"),
RateLimitPerMinute:   getEnvInt("RATE_LIMIT_PER_MINUTE", 120),
MessageMaxSize:       getEnvInt("MESSAGE_MAX_SIZE", 10240),
}
}

func getEnv(key, fallback string) string {
if value, exists := os.LookupEnv(key); exists {
return value
}
return fallback
}

func getEnvInt(key string, fallback int) int {
if value, exists := os.LookupEnv(key); exists {
if i, err := strconv.Atoi(value); err == nil {
return i
}
}
return fallback
}
