package providers

type Provider interface {
	Name() string
	BaseURL() string
	Model() string
	CostPer1KUSD() float64
}

type frontierProvider struct {
	name         string
	baseURL      string
	model        string
	apiKey       string
	costPer1KUSD float64
}

func (p *frontierProvider) Name() string         { return p.name }
func (p *frontierProvider) BaseURL() string      { return p.baseURL }
func (p *frontierProvider) Model() string        { return p.model }
func (p *frontierProvider) CostPer1KUSD() float64 { return p.costPer1KUSD }

func NewFrontierProvider(baseURL, model, apiKey string, costPer1KUSD float64) Provider {
	return &frontierProvider{
		name:         "frontier",
		baseURL:      baseURL,
		model:        model,
		apiKey:       apiKey,
		costPer1KUSD: costPer1KUSD,
	}
}

func NewZAIProvider(baseURL, model, apiKey string, costPer1KUSD float64) Provider {
	return &frontierProvider{
		name:         "zai",
		baseURL:      baseURL,
		model:        model,
		apiKey:       apiKey,
		costPer1KUSD: costPer1KUSD,
	}
}
