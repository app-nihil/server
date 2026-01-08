package redis

import (
"context"
"fmt"
"time"

goredis "github.com/redis/go-redis/v9"
)

const (
RateLimitWindow = 60 * time.Second
)

func (c *Client) CheckRateLimit(ctx context.Context, deviceUUID string, limit int) (int, bool, error) {
rateKey := fmt.Sprintf("rate:%s", deviceUUID)
now := time.Now().UnixMilli()
windowStart := now - int64(RateLimitWindow.Milliseconds())

c.rdb.ZRemRangeByScore(ctx, rateKey, "0", fmt.Sprintf("%d", windowStart))

count, err := c.rdb.ZCard(ctx, rateKey).Result()
if err != nil {
return 0, false, fmt.Errorf("failed to check rate limit: %w", err)
}

if int(count) >= limit {
return int(count), false, nil
}

c.rdb.ZAdd(ctx, rateKey, goredis.Z{
Score:  float64(now),
Member: fmt.Sprintf("%d", now),
})
c.rdb.Expire(ctx, rateKey, RateLimitWindow)

return int(count) + 1, true, nil
}

func (c *Client) RecordMessage(ctx context.Context, deviceUUID, messageHash string) error {
hashKey := fmt.Sprintf("msghash:%s:%s", deviceUUID, messageHash)
count, err := c.rdb.Incr(ctx, hashKey).Result()
if err != nil {
return err
}
c.rdb.Expire(ctx, hashKey, 5*time.Minute)

if count >= 10 {
return fmt.Errorf("spam detected")
}

timingKey := fmt.Sprintf("msgtiming:%s", deviceUUID)
now := time.Now().UnixMilli()

lastTime, err := c.rdb.Get(ctx, timingKey).Int64()
if err == nil {
if now-lastTime < 500 {
botKey := fmt.Sprintf("botcount:%s", deviceUUID)
botCount, _ := c.rdb.Incr(ctx, botKey).Result()
c.rdb.Expire(ctx, botKey, 5*time.Minute)

if botCount >= 20 {
return fmt.Errorf("bot-like behavior detected")
}
}
}

c.rdb.Set(ctx, timingKey, now, time.Minute)

return nil
}
