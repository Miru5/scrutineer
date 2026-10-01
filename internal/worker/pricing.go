package worker

import (
	"strings"

	"github.com/alpha-omega-security/harness"
)

const (
	modelDaybreakBlueID = "gpt-daybreak-blue-latest"
	modelGPT56SolID     = "gpt-5.6-sol"
	modelGPT6AstraID    = "gpt-6-astra"
	modelGPT6SolID      = "gpt-6-sol"
	modelGPT6LunaID     = "gpt-6-luna"
	perMillionTokens    = 1e6
)

// modelPrice is one model's standard base list price in USD per million tokens.
type modelPrice struct{ in, cachedIn, cacheWrite, out float64 }

// gpt6Pricing holds the GPT-6 family prices. The aggregate Usage event cannot
// identify requests that crossed the long-context threshold, so these
// deliberately remain base-rate estimates.
// https://developers.openai.com/api/docs/pricing
var gpt6Pricing = map[string]modelPrice{
	modelGPT6AstraID: {in: 10.00, cachedIn: 1.00, cacheWrite: 12.50, out: 50.00},
	modelGPT6SolID:   {in: 2.00, cachedIn: 0.20, cacheWrite: 2.50, out: 10.00},
	modelGPT6LunaID:  {in: 0.10, cachedIn: 0.01, cacheWrite: 0.125, out: 0.50},
}

// CostFromUsage computes the dollar cost of one result event's token usage
// against the given model's list price. Harness owns the shared pricing table;
// local handling bridges the GPT-6 family until the module ships its pricing.
func CostFromUsage(model string, u Usage) float64 {
	price, ok := gpt6Pricing[normalizePricingModelID(model)]
	if !ok {
		return harness.CostFromUsage(model, u)
	}

	uncached := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*price.in +
		float64(u.CacheReadTokens)*price.cachedIn +
		float64(u.CacheWriteTokens)*price.cacheWrite +
		float64(u.OutputTokens)*price.out) / perMillionTokens
}

func normalizePricingModelID(id string) string {
	if slash := strings.LastIndexByte(id, '/'); slash >= 0 {
		id = id[slash+1:]
	}
	if bracket := strings.IndexByte(id, '['); bracket > 0 {
		id = id[:bracket]
	}
	return id
}
