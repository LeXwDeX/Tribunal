package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/app"
)

type residueTestService struct {
	items        []app.ProjectResidue
	result       *app.ProjectResidueCleanup
	err          error
	listCalls    atomic.Int32
	cleanupCalls atomic.Int32
}

func (s *residueTestService) ListProjectResidues(context.Context) ([]app.ProjectResidue, error) {
	s.listCalls.Add(1)
	return s.items, s.err
}

func (s *residueTestService) CleanupProjectResidues(context.Context) (*app.ProjectResidueCleanup, error) {
	s.cleanupCalls.Add(1)
	return s.result, s.err
}

func newResidueServer(t *testing.T, residues ProjectResidueService) *httptest.Server {
	t.Helper()
	apiServer, err := New(Config{
		AdminToken: testAdminToken,
		Projects:   stubAdmin{},
		Residues:   residues,
		Webhooks:   stubWebhookAcceptor{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(apiServer.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func residueRequest(t *testing.T, ts *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func TestAdminListProjectResidues(t *testing.T) {
	service := &residueTestService{
		items: []app.ProjectResidue{{ProjectID: 42, Path: "/data/workspaces/42", Kind: app.ProjectResidueWorkspace, Removable: true}},
	}
	ts := newResidueServer(t, service)
	status, body := residueRequest(t, ts, http.MethodGet, "/api/v1/admin/projects/residues", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var response struct {
		Residues []app.ProjectResidue `json:"residues"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Residues) != 1 || response.Residues[0].ProjectID != 42 {
		t.Fatalf("residues = %+v", response.Residues)
	}
	if service.listCalls.Load() != 1 || service.cleanupCalls.Load() != 0 {
		t.Fatalf("calls = %d/%d", service.listCalls.Load(), service.cleanupCalls.Load())
	}
}

func TestAdminCleanupProjectResiduesRequiresConfirm(t *testing.T) {
	service := &residueTestService{result: &app.ProjectResidueCleanup{}}
	ts := newResidueServer(t, service)
	for _, body := range []string{"", `{}`, `{"confirm":false}`} {
		status, response := residueRequest(t, ts, http.MethodPost, "/api/v1/admin/projects/residues/cleanup", body)
		if status != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, response = %s", body, status, response)
		}
	}
	if service.cleanupCalls.Load() != 0 {
		t.Fatalf("cleanup called without confirm: %d", service.cleanupCalls.Load())
	}

	status, body := residueRequest(t, ts, http.MethodPost, "/api/v1/admin/projects/residues/cleanup", `{"confirm":true}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if service.cleanupCalls.Load() != 1 {
		t.Fatalf("cleanup calls = %d", service.cleanupCalls.Load())
	}
	var result app.ProjectResidueCleanup
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestAdminResiduesUnwired(t *testing.T) {
	ts := newResidueServer(t, nil)
	status, _ := residueRequest(t, ts, http.MethodGet, "/api/v1/admin/projects/residues", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("list status = %d", status)
	}
	status, _ = residueRequest(t, ts, http.MethodPost, "/api/v1/admin/projects/residues/cleanup", `{"confirm":true}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("cleanup status = %d", status)
	}
}

func TestAdminResiduesMapsSanitizedErrors(t *testing.T) {
	service := &residueTestService{err: errors.New("disk full at /secret/path")}
	ts := newResidueServer(t, service)
	status, body := residueRequest(t, ts, http.MethodGet, "/api/v1/admin/projects/residues", "")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if strings.Contains(string(body), "secret") || strings.Contains(string(body), "disk full") {
		t.Fatalf("error body leaked detail: %s", body)
	}
}
