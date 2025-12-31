package stripe

import (
"fmt"

"github.com/stripe/stripe-go/v82"
"github.com/stripe/stripe-go/v82/checkout/session"
)

var Plans = map[string]string{
"1_day_solo":    "price_1Sii7HCTw854gz2wyytgU8MC",
"1_week_solo":   "price_1Sii7rCTw854gz2wZONx1VOz",
"1_month_solo":  "price_1Sii8PCTw854gz2wCkEs5gz0",
"3_month_solo":  "price_1Sii98CTw854gz2w5hjbYHDD",
"1_year_solo":   "price_1Sii9XCTw854gz2weIhUN2yk",
"1_day_duo":     "price_1SiiAQCTw854gz2wY187RK3A",
"1_week_duo":    "price_1SiiAlCTw854gz2w7ZZlig6l",
"1_month_duo":   "price_1SiiBFCTw854gz2w4IYWL7Em",
"3_month_duo":   "price_1SiiBkCTw854gz2wX5meJ95i",
"1_year_duo":    "price_1SiiC6CTw854gz2w6HvqMj7c",
}

var PlanPrices = map[string]int64{
"1_day_solo":    500,
"1_week_solo":   1500,
"1_month_solo":  4900,
"3_month_solo":  12900,
"1_year_solo":   39900,
"1_day_duo":     800,
"1_week_duo":    2500,
"1_month_duo":   7900,
"3_month_duo":   19900,
"1_year_duo":    59900,
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

func (c *Client) GetCheckoutSession(sessionID string) (*stripe.CheckoutSession, error) {
return session.Get(sessionID, nil)
}

func IsPlanValid(plan string) bool {
_, ok := Plans[plan]
return ok
}

func GetPlanPrice(plan string) (int64, bool) {
price, ok := PlanPrices[plan]
return price, ok
}
