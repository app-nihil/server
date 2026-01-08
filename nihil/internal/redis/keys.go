package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Key bundle TTL - how long keys stay in Redis
const KeyBundleTTL = 30 * 24 * time.Hour // 30 days

// PreKey represents a one-time prekey
type PreKey struct {
	ID        int    `json:"id"`
	PublicKey string `json:"public_key"`
}

// SignedPreKey represents a signed prekey
type SignedPreKey struct {
	ID        int    `json:"id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

// KeyBundle represents a device's public key bundle
type KeyBundle struct {
	RegistrationID int          `json:"registration_id"`
	IdentityKey    string       `json:"identity_key"`
	SignedPreKey   SignedPreKey `json:"signed_prekey"`
	PreKey         *PreKey      `json:"prekey,omitempty"` // Single prekey for session establishment
}

// StoredKeyBundle is what we store (without prekeys - they're in separate HASH)
type StoredKeyBundle struct {
	RegistrationID int          `json:"registration_id"`
	IdentityKey    string       `json:"identity_key"`
	SignedPreKey   SignedPreKey `json:"signed_prekey"`
}

// Redis key helpers
func keyBundleKey(deviceUUID string) string {
	return fmt.Sprintf("keybundle:%s", deviceUUID)
}

func preKeysKey(deviceUUID string) string {
	return fmt.Sprintf("prekeys:%s", deviceUUID)
}

// StoreKeyBundle stores a device's key bundle and prekeys
// This REPLACES all existing prekeys - use for initial registration only
func (c *Client) StoreKeyBundle(ctx context.Context, deviceUUID string, registrationID int, identityKey string, signedPreKey SignedPreKey, preKeys []PreKey) error {
	// Store the main bundle (identity + signed prekey)
	bundle := StoredKeyBundle{
		RegistrationID: registrationID,
		IdentityKey:    identityKey,
		SignedPreKey:   signedPreKey,
	}

	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("marshal bundle: %w", err)
	}

	// Store bundle
	bundleKey := keyBundleKey(deviceUUID)
	if err := c.rdb.Set(ctx, bundleKey, bundleJSON, KeyBundleTTL).Err(); err != nil {
		return fmt.Errorf("store bundle: %w", err)
	}

	// Store prekeys in HASH - use Lua script for atomic replace-all
	if len(preKeys) > 0 {
		preKeysHashKey := preKeysKey(deviceUUID)

		// Build args for Lua script: key, ttl_seconds, id1, json1, id2, json2, ...
		args := make([]interface{}, 0, 2+len(preKeys)*2)
		args = append(args, int(KeyBundleTTL.Seconds()))

		for _, pk := range preKeys {
			pkJSON, err := json.Marshal(pk)
			if err != nil {
				return fmt.Errorf("marshal prekey %d: %w", pk.ID, err)
			}
			args = append(args, pk.ID, string(pkJSON))
		}

		// Lua script: delete existing, add all new prekeys atomically
		script := redis.NewScript(`
			local key = KEYS[1]
			local ttl = ARGV[1]
			
			-- Delete existing prekeys
			redis.call('DEL', key)
			
			-- Add new prekeys
			for i = 2, #ARGV, 2 do
				local id = ARGV[i]
				local data = ARGV[i + 1]
				redis.call('HSET', key, id, data)
			end
			
			-- Set TTL
			redis.call('EXPIRE', key, ttl)
			
			return #ARGV / 2 - 1
		`)

		_, err := script.Run(ctx, c.rdb, []string{preKeysHashKey}, args...).Result()
		if err != nil {
			return fmt.Errorf("store prekeys: %w", err)
		}
	}

	return nil
}

// AddPreKeys adds prekeys to existing HASH without deleting existing ones
// Use this for prekey replenishment
func (c *Client) AddPreKeys(ctx context.Context, deviceUUID string, preKeys []PreKey) error {
	if len(preKeys) == 0 {
		return nil
	}

	preKeysHashKey := preKeysKey(deviceUUID)

	// Use pipeline for efficiency
	pipe := c.rdb.Pipeline()

	for _, pk := range preKeys {
		pkJSON, err := json.Marshal(pk)
		if err != nil {
			return fmt.Errorf("marshal prekey %d: %w", pk.ID, err)
		}
		pipe.HSet(ctx, preKeysHashKey, fmt.Sprintf("%d", pk.ID), string(pkJSON))
	}

	// Refresh TTL
	pipe.Expire(ctx, preKeysHashKey, KeyBundleTTL)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("add prekeys: %w", err)
	}

	return nil
}

// GetKeyBundle retrieves a device's key bundle with ONE prekey (consumed atomically)
func (c *Client) GetKeyBundle(ctx context.Context, deviceUUID string) (*KeyBundle, error) {
	bundleKey := keyBundleKey(deviceUUID)

	// Get the main bundle
	bundleJSON, err := c.rdb.Get(ctx, bundleKey).Result()
	if err == redis.Nil {
		return nil, nil // No bundle found
	}
	if err != nil {
		return nil, fmt.Errorf("get bundle: %w", err)
	}

	var stored StoredKeyBundle
	if err := json.Unmarshal([]byte(bundleJSON), &stored); err != nil {
		return nil, fmt.Errorf("unmarshal bundle: %w", err)
	}

	bundle := &KeyBundle{
		RegistrationID: stored.RegistrationID,
		IdentityKey:    stored.IdentityKey,
		SignedPreKey:   stored.SignedPreKey,
	}

	// Consume one prekey atomically
	preKey, err := c.ConsumePreKey(ctx, deviceUUID)
	if err != nil {
		return nil, fmt.Errorf("consume prekey: %w", err)
	}

	if preKey != nil {
		bundle.PreKey = preKey
	}

	return bundle, nil
}

// ConsumePreKey atomically gets and removes one prekey from the HASH
// Returns nil if no prekeys available
func (c *Client) ConsumePreKey(ctx context.Context, deviceUUID string) (*PreKey, error) {
	preKeysHashKey := preKeysKey(deviceUUID)

	// Lua script: get random prekey, delete it, return it
	// This is atomic - no race conditions
	script := redis.NewScript(`
		local key = KEYS[1]
		
		-- Get all prekey IDs
		local ids = redis.call('HKEYS', key)
		if #ids == 0 then
			return nil
		end
		
		-- Pick first one (could randomize, but order doesn't matter for security)
		local id = ids[1]
		
		-- Get the prekey data
		local data = redis.call('HGET', key, id)
		
		-- Delete it
		redis.call('HDEL', key, id)
		
		return data
	`)

	result, err := script.Run(ctx, c.rdb, []string{preKeysHashKey}).Result()
	if err == redis.Nil || result == nil {
		return nil, nil // No prekeys available
	}
	if err != nil {
		return nil, fmt.Errorf("consume prekey script: %w", err)
	}

	// Parse the prekey JSON
	var preKey PreKey
	if err := json.Unmarshal([]byte(result.(string)), &preKey); err != nil {
		return nil, fmt.Errorf("unmarshal prekey: %w", err)
	}

	return &preKey, nil
}

// GetPreKeyCount returns the number of available prekeys for a device
func (c *Client) GetPreKeyCount(ctx context.Context, deviceUUID string) (int64, error) {
	preKeysHashKey := preKeysKey(deviceUUID)
	count, err := c.rdb.HLen(ctx, preKeysHashKey).Result()
	if err != nil {
		return 0, fmt.Errorf("get prekey count: %w", err)
	}
	return count, nil
}

// HasPreKey checks if a specific prekey ID exists
func (c *Client) HasPreKey(ctx context.Context, deviceUUID string, preKeyID int) (bool, error) {
	preKeysHashKey := preKeysKey(deviceUUID)
	exists, err := c.rdb.HExists(ctx, preKeysHashKey, fmt.Sprintf("%d", preKeyID)).Result()
	if err != nil {
		return false, fmt.Errorf("check prekey: %w", err)
	}
	return exists, nil
}

// DeleteKeyBundle removes a device's key bundle and all prekeys
func (c *Client) DeleteKeyBundle(ctx context.Context, deviceUUID string) error {
	bundleKey := keyBundleKey(deviceUUID)
	preKeysHashKey := preKeysKey(deviceUUID)

	pipe := c.rdb.Pipeline()
	pipe.Del(ctx, bundleKey)
	pipe.Del(ctx, preKeysHashKey)
	_, err := pipe.Exec(ctx)

	if err != nil {
		return fmt.Errorf("delete key bundle: %w", err)
	}
	return nil
}

// RefreshKeyBundleTTL refreshes the TTL on a device's keys
func (c *Client) RefreshKeyBundleTTL(ctx context.Context, deviceUUID string) error {
	bundleKey := keyBundleKey(deviceUUID)
	preKeysHashKey := preKeysKey(deviceUUID)

	pipe := c.rdb.Pipeline()
	pipe.Expire(ctx, bundleKey, KeyBundleTTL)
	pipe.Expire(ctx, preKeysHashKey, KeyBundleTTL)
	_, err := pipe.Exec(ctx)

	if err != nil {
		return fmt.Errorf("refresh TTL: %w", err)
	}
	return nil
}