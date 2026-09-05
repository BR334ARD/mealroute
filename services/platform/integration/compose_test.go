package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	platformapi "mealroute/platform/internal/api/platform"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type localVenueOrder struct {
	VenueOrderID    string    `json:"venueOrderId"`
	PlatformOrderID uuid.UUID `json:"platformOrderId"`
	Status          string    `json:"status"`
	RejectionReason *string   `json:"rejectionReason"`
	UpdatedAt       time.Time `json:"updatedAt"`
	Items           []struct {
		ExternalItemID string `json:"externalItemId"`
	} `json:"items"`
}

type localVenueOrderPage struct {
	Items []localVenueOrder `json:"items"`
}

func TestComposeDeliversCreatedAndCancelledOrderToVenue(t *testing.T) {
	if os.Getenv("COMPOSE_INTEGRATION") != "1" {
		t.Skip("set COMPOSE_INTEGRATION=1 to run the cross-service test")
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required when COMPOSE_INTEGRATION=1")
	}
	platformURL := strings.TrimRight(requiredEnvironment(t, "PLATFORM_BASE_URL"), "/")
	venueURL := strings.TrimRight(requiredEnvironment(t, "VENUE_BASE_URL"), "/")
	venueAPIKey := requiredEnvironment(t, "VENUE_API_KEY")
	client := &http.Client{Timeout: 3 * time.Second}

	waitForHealthy(t, client, platformURL+"/healthz")
	waitForHealthy(t, client, venueURL+"/v1/health")

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create cleanup pool: %v", err)
	}
	t.Cleanup(admin.Close)

	userID := "compose-integration-" + uuid.NewString()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		statements := []string{
			`DELETE FROM order_events WHERE order_id IN (SELECT id FROM orders WHERE customer_id = $1)`,
			`DELETE FROM idempotency_commands WHERE order_id IN (SELECT id FROM orders WHERE customer_id = $1)`,
			`DELETE FROM orders WHERE customer_id = $1`,
		}
		for _, statement := range statements {
			if _, err := admin.Exec(cleanupCtx, statement, userID); err != nil {
				t.Errorf("cleanup Compose fixture: %v", err)
			}
		}
	})

	venueID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	var menu platformapi.Menu
	status, err := sendJSON(client, http.MethodGet, platformURL+"/api/v1/venues/"+venueID.String()+"/menu", nil, nil, &menu)
	if err != nil || status != http.StatusOK || len(menu.Categories) == 0 || len(menu.Categories[0].Items) == 0 {
		t.Fatalf("get platform menu: status=%d err=%v menu=%+v", status, err, menu)
	}

	createRequest := platformapi.CreateOrderRequest{
		VenueId:     venueID,
		MenuVersion: menu.Version,
		Items:       []platformapi.OrderItemInput{{ProductId: menu.Categories[0].Items[0].Id, Quantity: 1}},
		DeliveryAddress: platformapi.DeliveryAddress{
			City:        "Новосибирск",
			AddressLine: "Compose-тест, 1",
		},
	}
	userHeaders := map[string]string{"X-User-Id": userID, "Idempotency-Key": "create-" + uuid.NewString()}
	var created platformapi.Order
	status, err = sendJSON(client, http.MethodPost, platformURL+"/api/v1/orders", createRequest, userHeaders, &created)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("create order: status=%d err=%v order=%+v", status, err, created)
	}

	pendingLocal := waitForVenueStatus(t, client, venueURL, created.Id, string(platformapi.OrderStatusPendingConfirmation))
	if pendingLocal.VenueOrderID == "" || pendingLocal.VenueOrderID == created.Id.String() {
		t.Fatalf("venue order ID must be present and distinct from platform ID: %+v", pendingLocal)
	}
	var acceptedByVenue localVenueOrder
	status, err = sendJSON(client, http.MethodPost, venueURL+"/v1/orders/"+url.PathEscape(pendingLocal.VenueOrderID)+"/status", map[string]any{"status": "accepted"}, nil, &acceptedByVenue)
	if err != nil || status != http.StatusOK || acceptedByVenue.Status != string(platformapi.OrderStatusAccepted) {
		t.Fatalf("venue accept order: status=%d err=%v order=%+v", status, err, acceptedByVenue)
	}
	waitForPlatformStatus(t, client, platformURL, userID, created.Id, platformapi.OrderStatusAccepted)
	acceptedLocal := waitForVenueStatus(t, client, venueURL, created.Id, string(platformapi.OrderStatusAccepted))
	if len(acceptedLocal.Items) != 1 || acceptedLocal.Items[0].ExternalItemID == "" || acceptedLocal.Items[0].ExternalItemID == menu.Categories[0].Items[0].Id.String() {
		t.Fatalf("venue did not preserve its external item ID: %+v", acceptedLocal.Items)
	}

	cancelHeaders := map[string]string{"X-User-Id": userID, "Idempotency-Key": "cancel-" + uuid.NewString()}
	var cancelled platformapi.Order
	status, err = sendJSON(client, http.MethodPost, platformURL+"/api/v1/orders/"+created.Id.String()+"/cancel", platformapi.CancelOrderRequest{}, cancelHeaders, &cancelled)
	if err != nil || status != http.StatusOK || cancelled.Status != platformapi.OrderStatusCancelled {
		t.Fatalf("cancel order: status=%d err=%v order=%+v", status, err, cancelled)
	}
	waitForVenueStatus(t, client, venueURL, created.Id, string(platformapi.OrderStatusCancelled))

	eventTypes := collectOrderEventTypes(t, client, platformURL, venueAPIKey, created.Id)
	for _, expected := range []platformapi.OrderEventType{platformapi.OrderEventTypeCreated, platformapi.OrderEventTypeAccepted, platformapi.OrderEventTypeCancelled} {
		if !eventTypes[expected] {
			t.Fatalf("event %q was not delivered to the partner journal: %+v", expected, eventTypes)
		}
	}
}

func TestComposeDeliversVenueRejectionEndToEnd(t *testing.T) {
	if os.Getenv("COMPOSE_INTEGRATION") != "1" {
		t.Skip("set COMPOSE_INTEGRATION=1 to run the cross-service test")
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required when COMPOSE_INTEGRATION=1")
	}
	platformURL := strings.TrimRight(requiredEnvironment(t, "PLATFORM_BASE_URL"), "/")
	venueURL := strings.TrimRight(requiredEnvironment(t, "VENUE_BASE_URL"), "/")
	venueAPIKey := requiredEnvironment(t, "VENUE_API_KEY")
	client := &http.Client{Timeout: 3 * time.Second}
	waitForHealthy(t, client, platformURL+"/healthz")
	waitForHealthy(t, client, venueURL+"/v1/health")

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create cleanup pool: %v", err)
	}
	t.Cleanup(admin.Close)
	userID := "compose-reject-" + uuid.NewString()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		statements := []string{
			`DELETE FROM order_events WHERE order_id IN (SELECT id FROM orders WHERE customer_id = $1)`,
			`DELETE FROM idempotency_commands WHERE order_id IN (SELECT id FROM orders WHERE customer_id = $1)`,
			`DELETE FROM orders WHERE customer_id = $1`,
		}
		for _, statement := range statements {
			if _, err := admin.Exec(cleanupCtx, statement, userID); err != nil {
				t.Errorf("cleanup reject fixture: %v", err)
			}
		}
	})

	venueID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	var menu platformapi.Menu
	status, err := sendJSON(client, http.MethodGet, platformURL+"/api/v1/venues/"+venueID.String()+"/menu", nil, nil, &menu)
	if err != nil || status != http.StatusOK || len(menu.Categories) == 0 || len(menu.Categories[0].Items) == 0 {
		t.Fatalf("get platform menu: status=%d err=%v menu=%+v", status, err, menu)
	}
	createRequest := platformapi.CreateOrderRequest{
		VenueId: venueID, MenuVersion: menu.Version,
		Items:           []platformapi.OrderItemInput{{ProductId: menu.Categories[0].Items[0].Id, Quantity: 1}},
		DeliveryAddress: platformapi.DeliveryAddress{City: "Новосибирск", AddressLine: "Compose reject, 1"},
	}
	var created platformapi.Order
	status, err = sendJSON(client, http.MethodPost, platformURL+"/api/v1/orders", createRequest, map[string]string{"X-User-Id": userID, "Idempotency-Key": "create-" + uuid.NewString()}, &created)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("create rejection order: status=%d err=%v order=%+v", status, err, created)
	}

	pendingLocal := waitForVenueStatus(t, client, venueURL, created.Id, string(platformapi.OrderStatusPendingConfirmation))
	if pendingLocal.VenueOrderID == "" || pendingLocal.VenueOrderID == created.Id.String() {
		t.Fatalf("venue order ID must be present and distinct from platform ID: %+v", pendingLocal)
	}
	rejectionReason := "kitchen capacity exhausted"
	var rejectedByVenue localVenueOrder
	status, err = sendJSON(client, http.MethodPost, venueURL+"/v1/orders/"+url.PathEscape(pendingLocal.VenueOrderID)+"/status", map[string]any{"status": "rejected", "reason": rejectionReason}, nil, &rejectedByVenue)
	if err != nil || status != http.StatusOK || rejectedByVenue.Status != string(platformapi.OrderStatusRejected) {
		t.Fatalf("venue reject order: status=%d err=%v order=%+v", status, err, rejectedByVenue)
	}
	waitForPlatformStatus(t, client, platformURL, userID, created.Id, platformapi.OrderStatusRejected)
	localOrder := waitForCanonicalVenueRejection(t, client, venueURL, created.Id, rejectedByVenue.UpdatedAt, rejectionReason)
	if localOrder.RejectionReason == nil || *localOrder.RejectionReason != rejectionReason {
		t.Fatalf("venue rejection reason was lost: %+v", localOrder)
	}
	eventTypes := collectOrderEventTypes(t, client, platformURL, venueAPIKey, created.Id)
	for _, expected := range []platformapi.OrderEventType{platformapi.OrderEventTypeCreated, platformapi.OrderEventTypeRejected} {
		if !eventTypes[expected] {
			t.Fatalf("event %q was not delivered for rejected order: %+v", expected, eventTypes)
		}
	}
}

func waitForCanonicalVenueRejection(t *testing.T, client *http.Client, venueURL string, platformOrderID uuid.UUID, after time.Time, expectedReason string) localVenueOrder {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last localVenueOrder
	for time.Now().Before(deadline) {
		var page localVenueOrderPage
		status, err := sendJSON(client, http.MethodGet, venueURL+"/v1/orders?limit=100", nil, nil, &page)
		if err == nil && status == http.StatusOK {
			for _, order := range page.Items {
				if order.PlatformOrderID != platformOrderID {
					continue
				}
				last = order
				if order.Status == string(platformapi.OrderStatusRejected) && order.UpdatedAt.After(after) && order.RejectionReason != nil && *order.RejectionReason == expectedReason {
					return order
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("canonical rejected snapshot was not applied for %s; last=%+v", platformOrderID, last)
	return localVenueOrder{}
}

func waitForHealthy(t *testing.T, client *http.Client, target string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(target)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("service did not become healthy: %s", target)
}

func waitForPlatformStatus(t *testing.T, client *http.Client, platformURL, userID string, orderID uuid.UUID, expected platformapi.OrderStatus) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last platformapi.Order
	for time.Now().Before(deadline) {
		status, err := sendJSON(client, http.MethodGet, platformURL+"/api/v1/orders/"+orderID.String(), nil, map[string]string{"X-User-Id": userID}, &last)
		if err == nil && status == http.StatusOK && last.Status == expected {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("platform order %s did not reach %s; last=%+v", orderID, expected, last)
}

func waitForVenueStatus(t *testing.T, client *http.Client, venueURL string, orderID uuid.UUID, expected string) localVenueOrder {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last localVenueOrder
	for time.Now().Before(deadline) {
		var page localVenueOrderPage
		status, err := sendJSON(client, http.MethodGet, venueURL+"/v1/orders?limit=100", nil, nil, &page)
		if err == nil && status == http.StatusOK {
			for _, order := range page.Items {
				if order.PlatformOrderID == orderID {
					last = order
					if order.Status == expected {
						return order
					}
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("venue order %s did not reach %s; last=%+v", orderID, expected, last)
	return localVenueOrder{}
}

func collectOrderEventTypes(t *testing.T, client *http.Client, platformURL, venueAPIKey string, orderID uuid.UUID) map[platformapi.OrderEventType]bool {
	t.Helper()
	result := make(map[platformapi.OrderEventType]bool)
	cursor := ""
	for pageNumber := 0; pageNumber < 20; pageNumber++ {
		target := platformURL + "/partner/v1/order-events?limit=100"
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		var page platformapi.OrderEventPage
		status, err := sendJSON(client, http.MethodGet, target, nil, map[string]string{"X-Venue-API-Key": venueAPIKey}, &page)
		if err != nil || status != http.StatusOK {
			t.Fatalf("list partner events: status=%d err=%v", status, err)
		}
		for _, event := range page.Items {
			if event.Order.Id == orderID {
				result[event.Type] = true
			}
		}
		if page.NextCursor == nil || len(page.Items) == 0 {
			return result
		}
		cursor = *page.NextCursor
	}
	t.Fatalf("partner event pagination did not terminate for order %s", orderID)
	return nil
}

func sendJSON(client *http.Client, method, target string, body any, headers map[string]string, responseBody any) (int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, target, reader)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("perform request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if responseBody != nil {
		if err := json.NewDecoder(response.Body).Decode(responseBody); err != nil {
			return response.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return response.StatusCode, nil
}

func requiredEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required for Compose integration tests", name)
	}
	return value
}
