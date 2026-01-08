package stripe

import (
	"fmt"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/price"
)

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

var PlanPrices = map[string]int64{
	"1_day_solo":   500,
	"1_week_solo":  1500,
	"1_month_solo": 4900,
	"3_month_solo": 12900,
	"1_year_solo":  39900,
	"1_day_duo":    800,
	"1_week_duo":   2500,
	"1_month_duo":  7900,
	"3_month_duo":  19900,
	"1_year_duo":   59900,
}

var SoloBasePrices = map[string]int64{
	"1_day":   500,
	"1_week":  1500,
	"1_month": 4900,
	"3_month": 12900,
	"1_year":  39900,
}

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

func (c *Client) ValidatePriceIDs(validatePrices bool) error {
	for plan, priceID := range Plans {
		p, err := price.Get(priceID, nil)
		if err != nil {
			return fmt.Errorf("price ID %s for plan %s not found in Stripe: %w", priceID, plan, err)
		}

		if !p.Active {
			return fmt.Errorf("price %s for plan %s is inactive", priceID, plan)
		}

		if validatePrices {
			expectedPrice, ok := PlanPrices[plan]
			if ok && p.UnitAmount != expectedPrice {
				return fmt.Errorf("price mismatch for %s", plan)
			}
		}
	}

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

	discountPercent := deviceCount + 18
	pricePerDevice := basePrice * int64(100-discountPercent) / 100
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