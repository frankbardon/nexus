package otel

// config holds parsed plugin configuration.
type config struct {
	endpoint      string   // OTLP endpoint (default "localhost:4317")
	protocol      string   // "grpc" or "http" (default "grpc")
	insecure      bool     // skip TLS verification (default true)
	serviceName   string   // OTel service name (default "nexus")
	excludeEvents []string // event types to skip
}

func parseConfig(raw map[string]any) config {
	cfg := config{
		endpoint:    "localhost:4317",
		protocol:    "grpc",
		insecure:    true,
		serviceName: "nexus",
	}

	if v, ok := raw["endpoint"]; ok {
		if s, ok := v.(string); ok {
			cfg.endpoint = s
		}
	}
	if v, ok := raw["protocol"]; ok {
		if s, ok := v.(string); ok && (s == "grpc" || s == "http") {
			cfg.protocol = s
		}
	}
	if v, ok := raw["insecure"]; ok {
		if b, ok := v.(bool); ok {
			cfg.insecure = b
		}
	}
	if v, ok := raw["service_name"]; ok {
		if s, ok := v.(string); ok {
			cfg.serviceName = s
		}
	}
	if v, ok := raw["exclude_events"]; ok {
		if list, ok := v.([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok {
					cfg.excludeEvents = append(cfg.excludeEvents, s)
				}
			}
		}
	}

	return cfg
}
