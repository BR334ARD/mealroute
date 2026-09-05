package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/httpapi"
	"mealroute/platform/internal/repository/memory"
	"mealroute/platform/internal/service"

	"github.com/google/uuid"
)

func TestOrderLifecycleThroughGeneratedHTTPRouter(t *testing.T) {
	t.Parallel()

	const venueAPIKey = "test-venue-api-key"
	application := service.New(memory.NewWithVenueAPIKey(venueAPIKey))
	router, err := httpapi.NewRouter(application)
	if err != nil {
		t.Fatalf("build test router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	venueID := uuid.MustParse(memory.DemoVenueID)
	var menu platformapi.Menu
	status := requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/venues/"+venueID.String()+"/menu", nil, nil, &menu)
	if status != http.StatusOK {
		t.Fatalf("get menu status: %d", status)
	}

	createRequest := platformapi.CreateOrderRequest{
		VenueId:     venueID,
		MenuVersion: menu.Version,
		Items:       []platformapi.OrderItemInput{{ProductId: menu.Categories[0].Items[0].Id, Quantity: 1}},
		DeliveryAddress: platformapi.DeliveryAddress{
			City:        "Новосибирск",
			AddressLine: "HTTP-тест, 1",
		},
	}
	userHeaders := map[string]string{
		"X-User-Id":       "http-test-user",
		"Idempotency-Key": "http-create-1",
	}
	var created platformapi.Order
	status = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders", createRequest, userHeaders, &created)
	if status != http.StatusCreated || created.Status != platformapi.OrderStatusPendingConfirmation {
		t.Fatalf("unexpected create response: status=%d order=%+v", status, created)
	}

	var customerPage platformapi.OrderPage
	status = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/api/v1/orders?limit=1", nil, map[string]string{"X-User-Id": "http-test-user"}, &customerPage)
	if status != http.StatusOK || len(customerPage.Items) != 1 || customerPage.Items[0].Id != created.Id {
		t.Fatalf("unexpected customer page: status=%d page=%+v", status, customerPage)
	}

	partnerHeaders := map[string]string{"X-Venue-API-Key": venueAPIKey}
	var createdEvents platformapi.OrderEventPage
	status = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/partner/v1/order-events?limit=20", nil, partnerHeaders, &createdEvents)
	if status != http.StatusOK || len(createdEvents.Items) != 1 || createdEvents.Items[0].Type != platformapi.OrderEventTypeCreated || createdEvents.NextCursor == nil {
		t.Fatalf("unexpected created events: status=%d page=%+v", status, createdEvents)
	}

	cancelHeaders := map[string]string{
		"X-User-Id":       "http-test-user",
		"Idempotency-Key": "http-cancel-1",
	}
	reason := "передумал"
	var cancelled platformapi.Order
	status = requestJSON(t, server.Client(), http.MethodPost, server.URL+"/api/v1/orders/"+created.Id.String()+"/cancel", platformapi.CancelOrderRequest{Reason: &reason}, cancelHeaders, &cancelled)
	if status != http.StatusOK || cancelled.Status != platformapi.OrderStatusCancelled {
		t.Fatalf("unexpected cancel response: status=%d order=%+v", status, cancelled)
	}

	var cancelledEvents platformapi.OrderEventPage
	eventsURL := server.URL + "/partner/v1/order-events?limit=20&cursor=" + url.QueryEscape(*createdEvents.NextCursor)
	status = requestJSON(t, server.Client(), http.MethodGet, eventsURL, nil, partnerHeaders, &cancelledEvents)
	if status != http.StatusOK || len(cancelledEvents.Items) != 1 || cancelledEvents.Items[0].Type != platformapi.OrderEventTypeCancelled || cancelledEvents.Items[0].Order.Id != created.Id {
		t.Fatalf("unexpected cancellation events: status=%d page=%+v", status, cancelledEvents)
	}

	var problem platformapi.ProblemDetails
	status = requestJSON(t, server.Client(), http.MethodGet, server.URL+"/partner/v1/order-events", nil, nil, &problem)
	if status != http.StatusUnauthorized || problem.Code != "invalid_venue_api_key" {
		t.Fatalf("expected partner authentication error, status=%d problem=%+v", status, problem)
	}
}

func TestRouterValidatesRequestsAndAlwaysReturnsJSONProblems(t *testing.T) {
	t.Parallel()

	router, err := httpapi.NewRouter(service.New(memory.New()))
	if err != nil {
		t.Fatalf("build test router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	tests := []struct {
		name    string
		method  string
		target  string
		body    any
		headers map[string]string
		status  int
		code    string
	}{
		{
			name:   "schema rejects unknown body field",
			method: http.MethodPost,
			target: server.URL + "/api/v1/orders",
			body: map[string]any{
				"venueId": "00000000-0000-0000-0000-000000000001", "menuVersion": 2,
				"items":           []map[string]any{{"productId": "00000000-0000-0000-0000-000000000011", "quantity": 1}},
				"deliveryAddress": map[string]any{"city": "Новосибирск", "addressLine": "Тестовая, 1"},
				"unexpected":      true,
			},
			headers: map[string]string{"X-User-Id": "validation-user", "Idempotency-Key": "validation-key"},
			status:  http.StatusBadRequest,
			code:    "invalid_request",
		},
		{name: "query constraint", method: http.MethodGet, target: server.URL + "/api/v1/venues?limit=101", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "missing required header", method: http.MethodGet, target: server.URL + "/api/v1/orders", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "method not allowed", method: http.MethodDelete, target: server.URL + "/api/v1/venues", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{name: "unknown route", method: http.MethodGet, target: server.URL + "/unknown", status: http.StatusNotFound, code: "route_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var problem platformapi.ProblemDetails
			status := requestJSON(t, server.Client(), test.method, test.target, test.body, test.headers, &problem)
			if status != test.status || problem.Code != test.code || problem.Message == "" {
				t.Fatalf("unexpected problem response: status=%d problem=%+v", status, problem)
			}
		})
	}
}

func TestPartnerMenuRejectsDuplicateExternalIDsAsBadRequest(t *testing.T) {
	t.Parallel()
	const venueAPIKey = "duplicate-menu-api-key"
	router, err := httpapi.NewRouter(service.New(memory.NewWithVenueAPIKey(venueAPIKey)))
	if err != nil {
		t.Fatalf("build test router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	request := platformapi.MenuSyncRequest{
		MenuVersion: 2,
		Categories: []platformapi.MenuCategoryInput{
			{ExternalCategoryId: "first", Name: "First", Items: []platformapi.MenuItemInput{{ExternalItemId: "duplicate", Name: "One", Price: platformapi.Money{Amount: 10000, Currency: platformapi.MoneyCurrencyRUB}, Available: true}}},
			{ExternalCategoryId: "second", Name: "Second", Items: []platformapi.MenuItemInput{{ExternalItemId: "duplicate", Name: "Two", Price: platformapi.Money{Amount: 20000, Currency: platformapi.MoneyCurrencyRUB}, Available: true}}},
		},
	}
	var problem platformapi.ProblemDetails
	status := requestJSON(t, server.Client(), http.MethodPut, server.URL+"/partner/v1/menu", request, map[string]string{"X-Venue-API-Key": venueAPIKey}, &problem)
	if status != http.StatusBadRequest || problem.Code != "invalid_request" {
		t.Fatalf("expected duplicate menu IDs to return 400 invalid_request, status=%d problem=%+v", status, problem)
	}
}

func requestJSON(t *testing.T, client *http.Client, method, target string, body any, headers map[string]string, responseBody any) int {
	t.Helper()

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, target, reader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("expected JSON response, got %q", response.Header.Get("Content-Type"))
	}
	if responseBody != nil {
		payload, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read response with status %d: %v", response.StatusCode, err)
		}
		if err := json.Unmarshal(payload, responseBody); err != nil {
			t.Fatalf("decode response with status %d: %v", response.StatusCode, err)
		}
	}
	return response.StatusCode
}
