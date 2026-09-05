package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	venueapi "mealroute/venue/internal/api/venue"

	"github.com/google/uuid"
)

func TestSyncMenuReturnsPlatformProductMappings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/partner/v1/menu" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Venue-API-Key") != "venue-key" {
			t.Fatalf("missing venue API key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"venueId":"00000000-0000-0000-0000-000000000001","menuVersion":1,"acceptedAt":"2026-08-28T00:00:00Z","items":[{"externalItemId":"pizza-pepperoni","productId":"00000000-0000-0000-0000-000000000011"}]}`))
	}))
	defer server.Close()

	result, err := New(server.URL, "venue-key", time.Second).SyncMenu(context.Background(), venueapi.VenueMenu{MenuVersion: 1})
	if err != nil {
		t.Fatalf("sync menu: %v", err)
	}
	if len(result.ProductMappings) != 1 {
		t.Fatalf("expected one product mapping, got %d", len(result.ProductMappings))
	}
	mapping := result.ProductMappings[0]
	if mapping.ExternalItemID != "pizza-pepperoni" || mapping.ProductID != "00000000-0000-0000-0000-000000000011" {
		t.Fatalf("unexpected product mapping: %+v", mapping)
	}
}

func TestRejectOrderCallsPartnerRejectEndpoint(t *testing.T) {
	orderID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/partner/v1/orders/"+orderID.String()+"/reject" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Venue-API-Key") != "venue-key" || r.Header.Get("Idempotency-Key") != "reject-key" {
			t.Fatalf("missing partner command headers")
		}
		var payload struct {
			Reason  string  `json:"reason"`
			Comment *string `json:"comment"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode reject request: %v", err)
		}
		if payload.Reason != "other" || payload.Comment == nil || *payload.Comment != "нет ингредиентов" {
			t.Fatalf("unexpected reject payload: %+v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + orderID.String() + `"}`))
	}))
	defer server.Close()

	reason := "нет ингредиентов"
	err := New(server.URL, "venue-key", time.Second).RejectOrder(context.Background(), venueapi.VenueOrder{PlatformOrderId: orderID}, &reason, "reject-key")
	if err != nil {
		t.Fatalf("reject order: %v", err)
	}
}

func TestPullOrderEventsMapsRejectionReason(t *testing.T) {
	orderID := uuid.New()
	eventID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"eventId":"` + eventID.String() + `","type":"rejected","occurredAt":"2026-08-28T00:00:01Z","order":{"id":"` + orderID.String() + `","status":"rejected","items":[],"total":{"amount":0,"currency":"RUB"},"rejectionReason":"нет ингредиентов","createdAt":"2026-08-28T00:00:00Z","updatedAt":"2026-08-28T00:00:01Z"}}]}`))
	}))
	defer server.Close()

	page, err := New(server.URL, "venue-key", time.Second).PullOrderEvents(context.Background(), "")
	if err != nil {
		t.Fatalf("pull order events: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Order.RejectionReason == nil || *page.Items[0].Order.RejectionReason != "нет ингредиентов" {
		t.Fatalf("rejection reason was not mapped: %+v", page)
	}
}
