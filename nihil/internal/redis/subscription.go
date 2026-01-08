package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type Subscription struct {
	DeviceUUID   string    `json:"device_uuid"`
	StripeSubID  string    `json:"stripe_sub_id"`
	Plan         string    `json:"plan"`
	PlanType     string    `json:"plan_type"`
	Status       string    `json:"status"`
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
	IsDuoGuest   bool      `json:"is_duo_guest"`
	DuoOwnerUUID string    `json:"duo_owner_uuid,omitempty"`
}

type ActivationCode struct {
	Code            string    `json:"code"`
	StripeSessionID string    `json:"stripe_session_id"`
	Plan            string    `json:"plan"`
	Type            string    `json:"type"` // "solo", "duo_owner", "duo_guest", "team"
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	DuoOwnerCode    string    `json:"duo_owner_code,omitempty"`
	// TEAM fields
	TeamIndex int    `json:"team_index,omitempty"` // 1, 2, 3... which code in the team
	TeamTotal int    `json:"team_total,omitempty"` // total devices in this team purchase
	Duration  string `json:"duration,omitempty"`   // for team: "1_day", "1_week", etc.
	// NOTE: ClaimedByDevice and ClaimedAt are deliberately NOT stored
	// This breaks the link between payment and device for privacy
}

func (c *Client) SetSubscription(ctx context.Context, sub *Subscription) error {
	subJSON, err := json.Marshal(sub)
	if err != nil {
		return fmt.Errorf("failed to marshal subscription: %w", err)
	}

	ttl := time.Until(sub.ExpiresAt)
	if ttl <= 0 {
		ttl = time.Hour
	}

	subKey := fmt.Sprintf("sub:%s", sub.DeviceUUID)
	if err := c.rdb.Set(ctx, subKey, subJSON, ttl).Err(); err != nil {
		return fmt.Errorf("failed to cache subscription: %w", err)
	}

	return nil
}

func (c *Client) GetSubscription(ctx context.Context, deviceUUID string) (*Subscription, error) {
	subKey := fmt.Sprintf("sub:%s", deviceUUID)
	subJSON, err := c.rdb.Get(ctx, subKey).Result()
	if err != nil {
		return nil, fmt.Errorf("subscription not found in cache: %w", err)
	}

	var sub Subscription
	if err := json.Unmarshal([]byte(subJSON), &sub); err != nil {
		return nil, fmt.Errorf("failed to unmarshal subscription: %w", err)
	}

	return &sub, nil
}

func (c *Client) IsSubscriptionActive(ctx context.Context, deviceUUID string) (bool, error) {
	sub, err := c.GetSubscription(ctx, deviceUUID)
	if err != nil {
		return false, nil
	}

	if sub.Status != "active" {
		return false, nil
	}

	if time.Now().After(sub.ExpiresAt) {
		return false, nil
	}

	return true, nil
}

func (c *Client) CreateActivationCode(ctx context.Context, code *ActivationCode) error {
	codeJSON, err := json.Marshal(code)
	if err != nil {
		return fmt.Errorf("failed to marshal activation code: %w", err)
	}

	codeKey := fmt.Sprintf("code:%s", code.Code)
	if err := c.rdb.Set(ctx, codeKey, codeJSON, 24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to store activation code: %w", err)
	}

	return nil
}

func (c *Client) GetActivationCode(ctx context.Context, code string) (*ActivationCode, error) {
	codeKey := fmt.Sprintf("code:%s", code)
	codeJSON, err := c.rdb.Get(ctx, codeKey).Result()
	if err != nil {
		return nil, fmt.Errorf("activation code not found: %w", err)
	}

	var ac ActivationCode
	if err := json.Unmarshal([]byte(codeJSON), &ac); err != nil {
		return nil, fmt.Errorf("failed to unmarshal activation code: %w", err)
	}

	return &ac, nil
}

func (c *Client) ClaimActivationCode(ctx context.Context, code, deviceUUID, publicKey string) (*Subscription, string, error) {
	ac, err := c.GetActivationCode(ctx, code)
	if err != nil {
		return nil, "", err
	}

	if ac.Status != "pending" {
		return nil, "", fmt.Errorf("activation code already used")
	}

	// Get duration based on plan type
	var duration time.Duration
	if ac.Type == "team" {
		duration = getTeamDuration(ac.Duration)
	} else {
		duration = getPlanDuration(ac.Plan)
	}

	// Check for existing subscription - ADD time instead of replace
	var expiresAt time.Time
	existingSub, err := c.GetSubscription(ctx, deviceUUID)
	if err == nil && existingSub.Status == "active" && existingSub.ExpiresAt.After(time.Now()) {
		// Add duration to existing expiry
		expiresAt = existingSub.ExpiresAt.Add(duration)
	} else {
		// New subscription starts now
		expiresAt = time.Now().Add(duration)
	}

	sub := &Subscription{
		DeviceUUID: deviceUUID,
		Plan:       ac.Plan,
		PlanType:   getPlanType(ac.Type),
		Status:     "active",
		ExpiresAt:  expiresAt,
		CreatedAt:  time.Now(),
		IsDuoGuest: ac.Type == "duo_guest",
	}

	if err := c.SetSubscription(ctx, sub); err != nil {
		return nil, "", err
	}

	keyKey := fmt.Sprintf("pubkey:%s", deviceUUID)
	c.rdb.Set(ctx, keyKey, publicKey, 0)

	// PRIVACY: Mark code as used but do NOT store which device claimed it
	// This breaks the Stripe payment -> device link
	ac.Status = "used"
	// REMOVED: ac.ClaimedByDevice = deviceUUID
	// REMOVED: ac.ClaimedAt = time.Now()
	codeJSON, _ := json.Marshal(ac)
	codeKey := fmt.Sprintf("code:%s", code)
	// Delete used code after short period (just for duplicate prevention)
	c.rdb.Set(ctx, codeKey, codeJSON, 1*time.Hour)

	// Remove from code pool
	c.RemoveFromCodePool(ctx, code)

	// Return session_id so app can store it for restoration
	return sub, ac.StripeSessionID, nil
}

// RestoreSubscription recreates a subscription using Stripe session verification
// Called when app has a stored session_id but server lost the subscription (restart)
func (c *Client) RestoreSubscription(ctx context.Context, deviceUUID, publicKey, plan, planType string, expiresAt time.Time) (*Subscription, error) {
	// Check if subscription already exists
	existingSub, err := c.GetSubscription(ctx, deviceUUID)
	if err == nil && existingSub.Status == "active" {
		return existingSub, nil
	}

	// Create new subscription
	sub := &Subscription{
		DeviceUUID: deviceUUID,
		Plan:       plan,
		PlanType:   planType,
		Status:     "active",
		ExpiresAt:  expiresAt,
		CreatedAt:  time.Now(),
	}

	if err := c.SetSubscription(ctx, sub); err != nil {
		return nil, err
	}

	// Restore public key
	keyKey := fmt.Sprintf("pubkey:%s", deviceUUID)
	c.rdb.Set(ctx, keyKey, publicKey, 0)

	return sub, nil
}

func (c *Client) GetDevicePublicKey(ctx context.Context, deviceUUID string) (string, error) {
	keyKey := fmt.Sprintf("pubkey:%s", deviceUUID)
	return c.rdb.Get(ctx, keyKey).Result()
}

func getPlanDuration(plan string) time.Duration {
	switch plan {
	case "1_day_solo", "1_day_duo":
		return 24 * time.Hour
	case "1_week_solo", "1_week_duo":
		return 7 * 24 * time.Hour
	case "1_month_solo", "1_month_duo":
		return 30 * 24 * time.Hour
	case "3_month_solo", "3_month_duo":
		return 90 * 24 * time.Hour
	case "1_year_solo", "1_year_duo":
		return 365 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

func getTeamDuration(duration string) time.Duration {
	switch duration {
	case "1_day":
		return 24 * time.Hour
	case "1_week":
		return 7 * 24 * time.Hour
	case "1_month":
		return 30 * 24 * time.Hour
	case "3_month":
		return 90 * 24 * time.Hour
	case "1_year":
		return 365 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

func getPlanType(codeType string) string {
	switch codeType {
	case "duo_owner", "duo_guest":
		return "duo"
	case "team":
		return "team"
	default:
		return "solo"
	}
}

func (c *Client) GetActivationCodesBySession(ctx context.Context, sessionID string) ([]ActivationCode, error) {
	// Use the code pool for faster lookup
	codes, err := c.GetCodesFromPool(ctx, sessionID)
	if err == nil && len(codes) > 0 {
		var result []ActivationCode
		for _, code := range codes {
			ac, err := c.GetActivationCode(ctx, code)
			if err == nil {
				result = append(result, *ac)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}

	// Fallback to scanning (for existing codes)
	keys, err := c.rdb.Keys(ctx, "code:*").Result()
	if err != nil {
		return nil, err
	}

	var result []ActivationCode
	for _, key := range keys {
		codeJSON, err := c.rdb.Get(ctx, key).Result()
		if err != nil {
			continue
		}

		var ac ActivationCode
		if err := json.Unmarshal([]byte(codeJSON), &ac); err != nil {
			continue
		}

		if ac.StripeSessionID == sessionID {
			result = append(result, ac)
		}
	}

	return result, nil
}

// ============================================
// ANONYMOUS CODE POOL
// Maps session_id -> codes for activation page lookup
// Does NOT map code -> device (that link is never stored)
// ============================================

func (c *Client) AddToCodePool(ctx context.Context, code, sessionID string) error {
	poolKey := fmt.Sprintf("pool:%s", sessionID)
	c.rdb.SAdd(ctx, poolKey, code)
	c.rdb.Expire(ctx, poolKey, 24*time.Hour)
	return nil
}

func (c *Client) GetCodesFromPool(ctx context.Context, sessionID string) ([]string, error) {
	poolKey := fmt.Sprintf("pool:%s", sessionID)
	return c.rdb.SMembers(ctx, poolKey).Result()
}

func (c *Client) RemoveFromCodePool(ctx context.Context, code string) error {
	// We don't know which session this code belongs to (by design)
	// The pool entry will expire naturally after 24h
	return nil
}