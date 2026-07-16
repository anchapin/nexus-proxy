package providers

// NewFrontierProvider returns a Provider configured for the primary frontier endpoint.
func NewFrontierProvider(baseURL, model, k string, costPer1KUSD float64) Provider {
	return Provider{
		Name:           "frontier",
		URL:            baseURL,
		Model:          model,
		APIKey:         k,
		InputCostPer1K: costPer1KUSD,
	}
}

// NewZAIProvider returns a Provider configured for the z.ai fallback endpoint.
func NewZAIProvider(baseURL, model, k string, costPer1KUSD float64) Provider {
	return Provider{
		Name:           "zai",
		URL:            baseURL,
		Model:          model,
		APIKey:         k,
		InputCostPer1K: costPer1KUSD,
	}
}
