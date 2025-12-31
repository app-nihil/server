package stripe

import (
	"fmt"
	"log"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/price"
)

// =============================================================================
// STRIPE PRICE ID CONFIGURATION
// =============================================================================
//
// IMPORTANT: These price IDs must match your Stripe Dashboard exactly.
// If you change prices in Stripe, update these values.
//
// To find price IDs:
// 1. Go to https://dashboard.stripe.com/products
// 2. Click on a product
// 3. Copy the price ID (starts with "price_")
//
// Last verified: December 2024
// =============================================================================

var Plans = map[string]string{
	"1_day_solo":   "price_1Sii7HCTw854gz2wyytgU8MC",
	"1_week_solo":  "price_1Sii7rCTw854gz2wZONx1VOz",
	"1_month_solo": "price_1Sii8PCTw854gz2wCkEs5gz0",
	"3_month_solo": "price_1Sii98CTw854gz2w5hjbYHDD",
	"1_year_solo":  "price_1Sii9XCTw854gz2weIhUN2yk",
	"1_day_duo":    "price_1SiiAQCTw854gz2wY187RK3A",
	"1_week_duo":   "price_1SiiAlCTw854gz2w7ZZlig6l",
	"1_month_duo":  "price_1SiiBFCTw854gz2w4IYWL7Em",
	"3_month_duo":  "price_1SiiBkCTw854gz2wX5meJ95i",
	"1_year_duo":   "price_1SiiC6CTw854gz2w6HvqMj7c",
}

// PlanPrices stores expected prices in cents for validation
// These should match the prices configured in Stripe
var PlanPrices = map[string]int64{
	"1_day_solo":   500,   // €5.00
	"1_week_solo":  1500,  // €15.00
	"1_month_solo": 4900,  // €49.00
	"3_month_solo": 12900, // €129.00
	"1_year_solo":  39900, // €399.00
	"1_day_duo":    800,   // €8.00
	"1_week_duo":   2500,  // €25.00
	"1_month_duo":  7900,  // €79.00
	"3_month_duo":  19900, // €199.00
	"1_year_duo":   59900, // €599.00
}

// SoloBasePrices in cents - used for TEAM dynamic pricing calculation
var SoloBasePrices = map[string]int64{
	"1_day":   500,
	"1_week":  1500,
	"1_month": 4900,
	"3_month": 12900,
	"1_year":  39900,
}

// DurationLabels for display in checkout
var DurationLabels = map[string]string{
	"1_day":   "1 Day",
	"1_week":  "1 Week",
	"1_month": "1 Month",
	"3_month": "3 Months",
	"1_year":  "1 Year",
}

var globalClient *Client

type Client struct {
	secretKey string
}

func NewClient(secretKey string) *Client {
	stripe.Key = secretKey
	globalClient = &Client{secretKey: secretKey}
	return globalClient
}

func GetClient() *Client {
	return globalClient
}

// ValidatePriceIDs checks that all configured price IDs exist in Stripe
// and optionally validates the prices match expected values.
// Call this on server startup to detect config drift early.
func (c *Client) ValidatePriceIDs(validatePrices bool) error {
	log.Println("Validating Stripe price IDs...")

	for plan, priceID := range Plans {
		p, err := price.Get(priceID, nil)
		if err != nil {
			return fmt.Errorf("price ID %s for plan %s not found in Stripe: %w", priceID, plan, err)
		}

		if !p.Active {
			log.Printf("WARNING: Price %s for plan %s is inactive in Stripe", priceID, plan)
		}

		if validatePrices {
			expectedPrice, ok := PlanPrices[plan]
			if ok && p.UnitAmount != expectedPrice {
				log.Printf("WARNING: Price mismatch for %s - expected %d cents, got %d cents",
					plan, expectedPrice, p.UnitAmount)
			}
		}
	}

	log.Println("Stripe price ID validation complete")
	return nil
}

func (c *Client) CreateCheckoutSession(plan string, successURL, cancelURL string) (*stripe.CheckoutSession, error) {
	priceID, ok := Plans[plan]
	if !ok || priceID == "" {
		return nil, fmt.Errorf("invalid plan: %s", plan)
	}

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		Metadata: map[string]string{
			"plan": plan,
		},
	}

	if len(plan) > 3 && plan[len(plan)-3:] == "duo" {
		params.Metadata["type"] = "duo"
	}

	return session.New(params)
}

// CreateTeamCheckoutSession creates a checkout session for TEAM plans with dynamic pricing
// TEAM plans use dynamic PriceData instead of fixed price IDs, avoiding config drift
func (c *Client) CreateTeamCheckoutSession(duration string, deviceCount int, successURL, cancelURL string) (*stripe.CheckoutSession, error) {
	basePrice, ok := SoloBasePrices[duration]
	if !ok {
		return nil, fmt.Errorf("invalid duration: %s", duration)
	}

	if deviceCount < 3 || deviceCount > 50 {
		return nil, fmt.Errorf("device count must be between 3 and 50")
	}

	durationLabel, ok := DurationLabels[duration]
	if !ok {
		return nil, fmt.Errorf("invalid duration: %s", duration)
	}

	// Calculate discount: device_count + 18 (so 3 devices = 21%, 50 devices = 68%)
	discountPercent := deviceCount + 18

	// Price per device with discount
	pricePerDevice := basePrice * int64(100-discountPercent) / 100

	// Total price
	totalPrice := pricePerDevice * int64(deviceCount)

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String("eur"),
					UnitAmount: stripe.Int64(totalPrice),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name:        stripe.String(fmt.Sprintf("TEAM - %s - %d devices", durationLabel, deviceCount)),
						Description: stripe.String(fmt.Sprintf("%d%% volume discount applied", discountPercent)),
					},
				},
				Quantity: stripe.Int64(1),
			},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		Metadata: map[string]string{
			"plan":         fmt.Sprintf("%s_team", duration),
			"type":         "team",
			"device_count": fmt.Sprintf("%d", deviceCount),
			"duration":     duration,
		},
	}

	return session.New(params)
}

func (c *Client) GetCheckoutSession(sessionID string) (*stripe.CheckoutSession, error) {
	return session.Get(sessionID, nil)
}

func IsPlanValid(plan string) bool {
	_, ok := Plans[plan]
	return ok
}

func IsTeamDurationValid(duration string) bool {
	_, ok := DurationLabels[duration]
	return ok
}

func GetPlanPrice(plan string) (int64, bool) {
	price, ok := PlanPrices[plan]
	return price, ok
}

// CalculateTeamPrice returns price per device and total price in cents
func CalculateTeamPrice(duration string, deviceCount int) (pricePerDevice int64, totalPrice int64, discountPercent int, err error) {
	basePrice, ok := SoloBasePrices[duration]
	if !ok {
		return 0, 0, 0, fmt.Errorf("invalid duration")
	}

	if deviceCount < 3 || deviceCount > 50 {
		return 0, 0, 0, fmt.Errorf("device count must be between 3 and 50")
	}

	discountPercent = deviceCount + 18
	pricePerDevice = basePrice * int64(100-discountPercent) / 100
	totalPrice = pricePerDevice * int64(deviceCount)

	return pricePerDevice, totalPrice, discountPercent, nil
}