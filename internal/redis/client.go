package redis

import (
"context"
"fmt"
"time"

"github.com/redis/go-redis/v9"
)

type Client struct {
rdb *redis.Client
}

func NewClient(redisURL string) (*Client, error) {
opt, err := redis.ParseURL(redisURL)
if err != nil {
return nil, fmt.Errorf("failed to parse redis URL: %w", err)
}

rdb := redis.NewClient(opt)

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := rdb.Ping(ctx).Err(); err != nil {
return nil, fmt.Errorf("failed to connect to redis: %w", err)
}

return &Client{rdb: rdb}, nil
}

func (c *Client) Close() error {
return c.rdb.Close()
}

func (c *Client) Ping(ctx context.Context) error {
return c.rdb.Ping(ctx).Err()
}

func (c *Client) GetRedis() *redis.Client {
return c.rdb
}
