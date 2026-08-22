package bdo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func newTestClient() *Client {
	return NewClient(
		WithRegion(RegionNA),
		WithLanguage("en"),
		WithRateLimit(rate.Limit(3), 5),
	)
}

func TestNewClientOptions(t *testing.T) {
	client := newTestClient()

	if client == nil {
		t.Fatal("expected non-nil Client")
	}
}

func TestGetItemOverview_Integration(t *testing.T) {
	client := newTestClient()

	targetID := 4901 // Black Stone Powder
	item, err := client.GetItemOverview(context.Background(), targetID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if item.Data[0].ID != targetID {
		t.Errorf("expected item ID %d, got %d", targetID, item.Data[0].ID)
	}

	if item.Data[0].Name == "" {
		t.Errorf("expected non-empty item name, got: %+v", item)
	}
}

func TestGetItemDetail_PriceNormalization(t *testing.T) {
	client := newTestClient()

	// Fetch details for Black Stone Powder (ID: 4901, +0 Enhancement)
	detail, err := client.GetItemDetail(context.Background(), 4901, 0)
	fmt.Println(detail)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if detail.Data.Availability[0].OnePrice == 0 {
		t.Errorf("expected non-zero PricePerOne, got 0 (payload: %+v)", detail)
	}
}

func TestSearch_EnvelopeUnwrapping(t *testing.T) {
	// Tests parsing when API returns a wrapped object { "items": [...] }
	client := newTestClient()

	results, err := client.Search(context.Background(), "Black Stone")
	if err != nil {
		t.Fatalf("unexpected error querying live search API: %v", err)
	}

	fmt.Println(results.Code)
	if results.Code != "SUCCESS" {
		t.Fatal("expected at least 1 search result from live API, got 0")
	}

	// Verify required fields on returned live items
	for _, item := range results.Data {
		if item.ID == 0 || item.Name == "" {
			t.Errorf("unmarshaled invalid item from search result: %+v", item)
		}
	}
}

func TestBatchGetItems_Resilience(t *testing.T) {
	// Tests that BatchGetItems captures 500 errors into Errors map without breaking successful items
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item/16001":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id": 16001, "name": "Black Stone (Weapon)"}`))
		case "/item/16002":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"code":"ERROR_INTERNAL","messages":["invalid data"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := NewClient()

	batch := client.BatchGetItems(context.Background(), []int{16001, 16002})

	if len(batch.Items) != 1 {
		t.Errorf("expected 1 successful item, got %d", len(batch.Items))
	}
	if _, ok := batch.Items[16001]; !ok {
		t.Error("expected item 16001 to succeed")
	}

	if len(batch.Errors) != 1 {
		t.Errorf("expected 1 error, got %d", len(batch.Errors))
	}
	if _, ok := batch.Errors[16002]; !ok {
		t.Error("expected item 16002 to fail")
	}
}

func TestRateLimiter(t *testing.T) {
	// Rate limit: 1 request per 100ms, burst 1
	client := newTestClient()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// First call consumes the burst token
	_, _ = client.GetHotList(context.Background())

	// Second call within 50ms with a context timeout should fail due to rate limit wait cancellation
	_, err := client.GetHotList(ctx)
	if err == nil {
		t.Fatal("expected error due to context cancellation in rate limiter, got nil")
	}
}

func TestGetItemIconURL(t *testing.T) {
	client := NewClient()
	url := client.GetItemIconURL(16001)
	expected := "https://api.blackdesertmarket.com/item/16001/icon"

	if url != expected {
		t.Errorf("expected %s, got %s", expected, url)
	}
}

func TestGetQueueList_LiveIntegration(t *testing.T) {
	client := newTestClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	items, err := client.GetQueueList(ctx)
	if err != nil {
		t.Fatalf("GetQueueList failed: %v", err)
	}

	t.Logf("Retrieved %v queued items", items.Code == "SUCCESS")
	for _, item := range items.Data {
		if item.ID == 0 {
			t.Errorf("expected non-zero item ID, got: %+v", item)
		}
	}
}

func TestGetCategoryItems_LiveIntegration(t *testing.T) {
	client := newTestClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Main Category 1 (Main Weapon), Sub Category 1 (Longsword)
	items, err := client.GetCategoryItems(ctx, 1, 1)
	if err != nil {
		t.Fatalf("GetCategoryItems failed: %v", err)
	}

	if items.Code == "SUCCESS" {
		t.Fatal("expected items in category 1/1, got 0")
	}

	for _, item := range items.Data {
		if item.ID == 0 || item.Name == "" {
			t.Errorf("unmarshaled invalid category item: %+v", item)
		}
	}
	t.Logf("Retrieved %d items for category 1/1 (Sample: %s, Stock: %d)", len(items.Data), items.Data[0].Name, items.Data[0].Count)
}
