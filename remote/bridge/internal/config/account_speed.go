package config

// AccountSpeedMode preserves the legacy absence as the standard policy: leave
// the client's service tier alone and let the provider choose its default.
func AccountSpeedMode(value string) string {
	if value == "" {
		return "standard"
	}
	return value
}

func ValidAccountSpeedMode(value string) bool {
	switch value {
	case "", "standard", "fast", "ultrafast":
		return true
	default:
		return false
	}
}

// AccountServiceTier is an explicit request, never evidence of entitlement or
// actual speed. Responses and Chat Completions use these protocol values;
// Anthropic and image endpoints have different contracts and must not use it.
func AccountServiceTier(mode string) string {
	switch mode {
	case "fast":
		return "priority"
	case "ultrafast":
		return "ultrafast"
	default:
		return ""
	}
}
