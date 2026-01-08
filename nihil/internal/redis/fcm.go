package redis

import (
	"context"
	"fmt"
)

// StoreFCMToken saves the FCM token for a device
func (c *Client) StoreFCMToken(ctx context.Context, deviceUUID, fcmToken string) error {
	key := fmt.Sprintf("fcm:%s", deviceUUID)
	return c.rdb.Set(ctx, key, fcmToken, 0).Err() // No expiry
}

// GetFCMToken retrieves the FCM token for a device
func (c *Client) GetFCMToken(ctx context.Context, deviceUUID string) (string, error) {
	key := fmt.Sprintf("fcm:%s", deviceUUID)
	return c.rdb.Get(ctx, key).Result()
}

// DeleteFCMToken removes the FCM token for a device
func (c *Client) DeleteFCMToken(ctx context.Context, deviceUUID string) error {
	key := fmt.Sprintf("fcm:%s", deviceUUID)
	return c.rdb.Del(ctx, key).Err()
}
