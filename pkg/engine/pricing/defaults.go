package pricing

// Embedded default price tables. Update on patch releases when providers
// change rates. Config overrides via Table.Merge always win.
//
// All values are USD per million tokens.

var anthropicDefaults = map[string]Rates{
	// Current Claude 4.x family (the canonical IDs accepted by the API).
	"claude-opus-4-7":           {InputPerMillion: 15.0, OutputPerMillion: 75.0},
	"claude-sonnet-4-6":         {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-haiku-4-5-20251001": {InputPerMillion: 0.80, OutputPerMillion: 4.0},
	// Older 4.x snapshots kept for sessions that pin a specific dated build.
	"claude-sonnet-4-20250514": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-opus-4-20250514":   {InputPerMillion: 15.0, OutputPerMillion: 75.0},
	// Legacy 3.x family — present so historical journals still cost-attribute.
	"claude-3-5-sonnet-20241022": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-3-5-haiku-20241022":  {InputPerMillion: 0.80, OutputPerMillion: 4.0},
	"claude-3-opus-20240229":     {InputPerMillion: 15.0, OutputPerMillion: 75.0},
}

var openaiDefaults = map[string]Rates{
	"gpt-4o":            {InputPerMillion: 2.50, OutputPerMillion: 10.0},
	"gpt-4o-mini":       {InputPerMillion: 0.15, OutputPerMillion: 0.60},
	"gpt-4o-2024-11-20": {InputPerMillion: 2.50, OutputPerMillion: 10.0},
	"gpt-4-turbo":       {InputPerMillion: 10.0, OutputPerMillion: 30.0},
	"gpt-4":             {InputPerMillion: 30.0, OutputPerMillion: 60.0},
	"gpt-3.5-turbo":     {InputPerMillion: 0.50, OutputPerMillion: 1.50},
	"o1":                {InputPerMillion: 15.0, OutputPerMillion: 60.0},
	"o1-mini":           {InputPerMillion: 3.0, OutputPerMillion: 12.0},
	"o3":                {InputPerMillion: 10.0, OutputPerMillion: 40.0},
	"o3-mini":           {InputPerMillion: 1.10, OutputPerMillion: 4.40},
	"o4-mini":           {InputPerMillion: 1.10, OutputPerMillion: 4.40},
}

var geminiDefaults = map[string]Rates{
	"gemini-2.5-pro":        {InputPerMillion: 1.25, OutputPerMillion: 10.00, CachedRatio: 0.25},
	"gemini-2.5-flash":      {InputPerMillion: 0.30, OutputPerMillion: 2.50, CachedRatio: 0.25},
	"gemini-2.5-flash-lite": {InputPerMillion: 0.10, OutputPerMillion: 0.40, CachedRatio: 0.25},
	"gemini-2.0-flash":      {InputPerMillion: 0.10, OutputPerMillion: 0.40, CachedRatio: 0.25},
	"gemini-2.0-flash-lite": {InputPerMillion: 0.075, OutputPerMillion: 0.30, CachedRatio: 0.25},
	"gemini-1.5-pro":        {InputPerMillion: 1.25, OutputPerMillion: 5.00, CachedRatio: 0.25},
	"gemini-1.5-flash":      {InputPerMillion: 0.075, OutputPerMillion: 0.30, CachedRatio: 0.25},
	"gemini-1.5-flash-8b":   {InputPerMillion: 0.0375, OutputPerMillion: 0.15, CachedRatio: 0.25},
}
