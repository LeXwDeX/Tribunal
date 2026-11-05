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
