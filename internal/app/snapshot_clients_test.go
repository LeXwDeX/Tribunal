package app

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

func TestSnapshotClientsUsesCatalogGeneration(t *testing.T) {
	catalog, err := llm.NewCatalog(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := &ReviewService{catalog: catalog}
	if _, ok := service.snapshotClients().(*llm.CatalogSnapshot); !ok {
		t.Fatalf("clients = %T, want *llm.CatalogSnapshot", service.snapshotClients())
	}
	if (&ReviewService{}).snapshotClients() != nil {
		t.Fatal("nil catalog must keep the injected clients")
	}
}
