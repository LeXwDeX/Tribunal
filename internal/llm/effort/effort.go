// Package effort defines the supported reasoning effort values.
package effort

// Valid reports whether value is a supported reasoning effort.
func Valid(value string) bool {
	switch value {
	case "", "low", "medium", "high", "max":
		return true
	default:
		return false
	}
}

// ValidCapability reports whether value is a supported reasoning effort for a
// provider. "none" is accepted only when the provider declares
// allow_reasoning_effort_none, so existing providers keep the closed vocabulary.
func ValidCapability(value string, allowNone bool) bool {
	if allowNone && value == "none" {
		return true
	}
	return Valid(value)
}

// ValidWire reports whether value may appear in audit metadata. It includes
// "none" because a capability-declaring provider sends it, and the audit keeps
// what the adapter actually sent.
func ValidWire(value string) bool {
	return value == "none" || Valid(value)
}
