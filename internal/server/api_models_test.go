package server

import (
	"net/http"
	"testing"
)

func TestModelEndpointsRequirePlatformAdminAndCSRF(t *testing.T) {
	env := newM6Env(t, m6Options{})
	viewer := env.loginAs("code-viewer")
	adminWithoutCSRF := env.loginAs("code-admin").withoutCSRF()
	endpoints := []struct {
		name, method, path string
	}{
		{"list", http.MethodGet, "/api/v1/models"},
		{"detail", http.MethodGet, "/api/v1/models/user:missing"},
		{"create", http.MethodPost, "/api/v1/models"},
		{"update", http.MethodPatch, "/api/v1/models/user:missing"},
		{"delete", http.MethodDelete, "/api/v1/models/user:missing?revision=1"},
		{"saved test", http.MethodPost, "/api/v1/models/user:missing/test"},
		{"draft test", http.MethodPost, "/api/v1/models:test"},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			response := viewer.request(endpoint.method, endpoint.path, nil, nil)
			if response.status != http.StatusForbidden {
				t.Errorf("viewer status = %d, want 403", response.status)
			}
			if !safeMethod(endpoint.method) {
				response = adminWithoutCSRF.request(endpoint.method, endpoint.path, nil, nil)
				if response.status != http.StatusForbidden {
					t.Errorf("session without CSRF status = %d, want 403", response.status)
				}
			}
		})
	}
}
