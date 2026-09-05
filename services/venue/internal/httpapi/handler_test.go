package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	venueapi "mealroute/venue/internal/api/venue"
	"mealroute/venue/internal/httpapi"
	"mealroute/venue/internal/repository/memory"
	"mealroute/venue/internal/service"
)

func TestRouterValidatesVenueRequestsAndReturnsJSONProblems(t *testing.T) {
	t.Parallel()

	router, err := httpapi.NewRouter(service.New(memory.New(), nil))
	if err != nil {
		t.Fatalf("build venue router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	tests := []struct {
		name   string
		method string
		target string
		body   any
		status int
		code   string
	}{
		{name: "query limit", method: http.MethodGet, target: server.URL + "/v1/orders?limit=101", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "non-command status", method: http.MethodPost, target: server.URL + "/v1/orders/unknown/status", body: map[string]any{"status": "cancelled"}, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "invalid cursor", method: http.MethodGet, target: server.URL + "/v1/orders?cursor=not-a-cursor", status: http.StatusBadRequest, code: "invalid_request"},
		{name: "method not allowed", method: http.MethodDelete, target: server.URL + "/v1/orders", status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{name: "unknown route", method: http.MethodGet, target: server.URL + "/unknown", status: http.StatusNotFound, code: "route_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var problem venueapi.ProblemDetails
			status := venueRequestJSON(t, server.Client(), test.method, test.target, test.body, &problem)
			if status != test.status || problem.Code != test.code || problem.Message == "" {
				t.Fatalf("unexpected problem response: status=%d problem=%+v", status, problem)
			}
		})
	}
}

func venueRequestJSON(t *testing.T, client *http.Client, method, target string, body, responseBody any) int {
	t.Helper()

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, target, reader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("expected JSON response, got %q", response.Header.Get("Content-Type"))
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if err := json.Unmarshal(payload, responseBody); err != nil {
		t.Fatalf("decode response with status %d: %v", response.StatusCode, err)
	}
	return response.StatusCode
}
