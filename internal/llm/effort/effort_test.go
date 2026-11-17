package effort

import "testing"

func TestValid(t *testing.T) {
	for _, value := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		if !Valid(value) {
			t.Errorf("Valid(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"XHigh", "x-high", "none", "ultra", "max "} {
		if Valid(value) {
			t.Errorf("Valid(%q) = true, want false", value)
		}
	}
}

func TestValidCapability(t *testing.T) {
	if !ValidCapability("none", true) {
		t.Error("ValidCapability(none, true) = false, want true")
	}
	if ValidCapability("none", false) {
		t.Error("ValidCapability(none, false) = true, want false")
	}
	if !ValidCapability("xhigh", false) {
		t.Error("ValidCapability(xhigh, false) = false, want true")
	}
}

func TestValidWire(t *testing.T) {
	for _, value := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
		if !ValidWire(value) {
			t.Errorf("ValidWire(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"XHigh", "x-high", "ultra"} {
		if ValidWire(value) {
			t.Errorf("ValidWire(%q) = true, want false", value)
		}
	}
}
