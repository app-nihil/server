package redis

import (
"context"
"fmt"
)

func (c *Client) PurgeDevice(ctx context.Context, deviceUUID string) error {
keysToDelete := []string{
fmt.Sprintf("sub:%s", deviceUUID),
fmt.Sprintf("pubkey:%s", deviceUUID),
fmt.Sprintf("keys:%s", deviceUUID),
fmt.Sprintf("fcm:%s", deviceUUID),
fmt.Sprintf("prekeys:%s", deviceUUID),
fmt.Sprintf("rate:%s", deviceUUID),
fmt.Sprintf("warn:%s", deviceUUID),
}

userChatsKey := fmt.Sprintf("user_chats:%s", deviceUUID)
chatUUIDs, _ := c.rdb.SMembers(ctx, userChatsKey).Result()

for _, chatUUID := range chatUUIDs {
c.rdb.Del(ctx, fmt.Sprintf("chat:%s", chatUUID))
c.rdb.Del(ctx, fmt.Sprintf("invitation:%s", chatUUID))
msgQueueKey := fmt.Sprintf("msg_queue:%s", chatUUID)
msgIDs, _ := c.rdb.LRange(ctx, msgQueueKey, 0, -1).Result()
for _, msgID := range msgIDs {
c.rdb.Del(ctx, fmt.Sprintf("msg:%s:%s", chatUUID, msgID))
}
c.rdb.Del(ctx, msgQueueKey)
}

keysToDelete = append(keysToDelete, userChatsKey)
for _, key := range keysToDelete {
c.rdb.Del(ctx, key)
}

return nil
}
