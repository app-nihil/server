package redis

import (
"context"
"encoding/json"
"fmt"
)

type KeyBundle struct {
DeviceUUID     string       `json:"device_uuid"`
RegistrationID int          `json:"registration_id"`
IdentityKey    string       `json:"identity_key"`
SignedPreKey   SignedPreKey `json:"signed_prekey"`
PreKeys        []PreKey     `json:"prekeys"`
}

type SignedPreKey struct {
ID        int    `json:"id"`
PublicKey string `json:"public_key"`
Signature string `json:"signature"`
}

type PreKey struct {
ID        int    `json:"id"`
PublicKey string `json:"public_key"`
}

func (c *Client) StoreKeyBundle(ctx context.Context, bundle *KeyBundle) error {
// Store identity key and signed prekey
bundleKey := fmt.Sprintf("keybundle:%s", bundle.DeviceUUID)
bundleData, err := json.Marshal(map[string]interface{}{
"registration_id": bundle.RegistrationID,
"identity_key":    bundle.IdentityKey,
"signed_prekey":   bundle.SignedPreKey,
})
if err != nil {
return err
}

if err := c.rdb.Set(ctx, bundleKey, bundleData, 0).Err(); err != nil {
return err
}

// Store prekeys as a list
preKeysKey := fmt.Sprintf("prekeys:%s", bundle.DeviceUUID)

// Delete existing prekeys
c.rdb.Del(ctx, preKeysKey)

// Add new prekeys
for _, pk := range bundle.PreKeys {
pkData, _ := json.Marshal(pk)
c.rdb.RPush(ctx, preKeysKey, pkData)
}

return nil
}

func (c *Client) GetKeyBundle(ctx context.Context, deviceUUID string) (*KeyBundle, error) {
bundleKey := fmt.Sprintf("keybundle:%s", deviceUUID)

data, err := c.rdb.Get(ctx, bundleKey).Bytes()
if err != nil {
return nil, err
}

var bundleData struct {
RegistrationID int          `json:"registration_id"`
IdentityKey    string       `json:"identity_key"`
SignedPreKey   SignedPreKey `json:"signed_prekey"`
}

if err := json.Unmarshal(data, &bundleData); err != nil {
return nil, err
}

return &KeyBundle{
DeviceUUID:     deviceUUID,
RegistrationID: bundleData.RegistrationID,
IdentityKey:    bundleData.IdentityKey,
SignedPreKey:   bundleData.SignedPreKey,
}, nil
}

func (c *Client) ConsumePreKey(ctx context.Context, deviceUUID string) (*PreKey, error) {
preKeysKey := fmt.Sprintf("prekeys:%s", deviceUUID)

// Pop from the left (FIFO)
data, err := c.rdb.LPop(ctx, preKeysKey).Bytes()
if err != nil {
return nil, err
}

var preKey PreKey
if err := json.Unmarshal(data, &preKey); err != nil {
return nil, err
}

return &preKey, nil
}

func (c *Client) AddPreKeys(ctx context.Context, deviceUUID string, preKeys []PreKey) error {
preKeysKey := fmt.Sprintf("prekeys:%s", deviceUUID)

for _, pk := range preKeys {
pkData, _ := json.Marshal(pk)
if err := c.rdb.RPush(ctx, preKeysKey, pkData).Err(); err != nil {
return err
}
}

return nil
}

func (c *Client) GetPreKeyCount(ctx context.Context, deviceUUID string) (int64, error) {
preKeysKey := fmt.Sprintf("prekeys:%s", deviceUUID)
return c.rdb.LLen(ctx, preKeysKey).Result()
}
