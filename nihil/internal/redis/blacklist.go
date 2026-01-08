package redis

import (
"context"
"encoding/json"
"fmt"
"time"
)

const (
WarningExpiry = 24 * time.Hour
)

type Ban struct {
DeviceUUID string    `json:"device_uuid"`
Reason     string    `json:"reason"`
BannedAt   time.Time `json:"banned_at"`
}

type Warning struct {
DeviceUUID  string    `json:"device_uuid"`
Reason      string    `json:"reason"`
Count       int       `json:"count"`
LastWarning time.Time `json:"last_warning"`
}

func (c *Client) IsBanned(ctx context.Context, deviceUUID string) (bool, string, error) {
banKey := fmt.Sprintf("ban:%s", deviceUUID)
banJSON, err := c.rdb.Get(ctx, banKey).Result()
if err != nil {
return false, "", nil
}

var ban Ban
if err := json.Unmarshal([]byte(banJSON), &ban); err != nil {
return false, "", nil
}

return true, ban.Reason, nil
}

func (c *Client) BanDevice(ctx context.Context, deviceUUID, reason string) error {
ban := Ban{
DeviceUUID: deviceUUID,
Reason:     reason,
BannedAt:   time.Now(),
}

banJSON, err := json.Marshal(ban)
if err != nil {
return fmt.Errorf("failed to marshal ban: %w", err)
}

banKey := fmt.Sprintf("ban:%s", deviceUUID)
if err := c.rdb.Set(ctx, banKey, banJSON, 0).Err(); err != nil {
return fmt.Errorf("failed to ban device: %w", err)
}

c.rdb.Del(ctx, fmt.Sprintf("warn:%s", deviceUUID))
c.rdb.Del(ctx, fmt.Sprintf("rate:%s", deviceUUID))

return nil
}

func (c *Client) GetWarning(ctx context.Context, deviceUUID string) (*Warning, error) {
warnKey := fmt.Sprintf("warn:%s", deviceUUID)
warnJSON, err := c.rdb.Get(ctx, warnKey).Result()
if err != nil {
return nil, nil
}

var warning Warning
if err := json.Unmarshal([]byte(warnJSON), &warning); err != nil {
return nil, err
}

return &warning, nil
}

func (c *Client) AddWarning(ctx context.Context, deviceUUID, reason string) (bool, error) {
warning, _ := c.GetWarning(ctx, deviceUUID)

if warning != nil && warning.Count >= 1 {
return true, nil
}

newWarning := Warning{
DeviceUUID:  deviceUUID,
Reason:      reason,
Count:       1,
LastWarning: time.Now(),
}

if warning != nil {
newWarning.Count = warning.Count + 1
}

warnJSON, err := json.Marshal(newWarning)
if err != nil {
return false, fmt.Errorf("failed to marshal warning: %w", err)
}

warnKey := fmt.Sprintf("warn:%s", deviceUUID)
if err := c.rdb.Set(ctx, warnKey, warnJSON, WarningExpiry).Err(); err != nil {
return false, fmt.Errorf("failed to store warning: %w", err)
}

return false, nil
}

func (c *Client) HandleAbuse(ctx context.Context, deviceUUID, reason string) (string, error) {
banned, _, _ := c.IsBanned(ctx, deviceUUID)
if banned {
return "ban", nil
}

shouldBan, err := c.AddWarning(ctx, deviceUUID, reason)
if err != nil {
return "", err
}

if shouldBan {
if err := c.BanDevice(ctx, deviceUUID, reason); err != nil {
return "", err
}
return "ban", nil
}

return "warning", nil
}
