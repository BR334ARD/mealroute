// Package platform contains the HTTP implementation of the venue's outbound
// platform gateway.
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	venueapi "mealroute/venue/internal/api/venue"
	"mealroute/venue/internal/repository"
	"mealroute/venue/internal/service"

	"github.com/google/uuid"
)

type Client struct {
	baseURL     string
	venueAPIKey string
	httpClient  *http.Client
}

func New(baseURL, venueAPIKey string, timeout time.Duration) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), venueAPIKey: venueAPIKey, httpClient: &http.Client{Timeout: timeout}}
}

func (c *Client) SyncMenu(ctx context.Context, menu venueapi.VenueMenu) (service.MenuSyncResult, error) {
	payload := struct {
		MenuVersion int64                        `json:"menuVersion"`
		Categories  []venueapi.VenueMenuCategory `json:"categories"`
	}{MenuVersion: menu.MenuVersion, Categories: menu.Categories}
	body, err := json.Marshal(payload)
	if err != nil {
		return service.MenuSyncResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/partner/v1/menu", bytes.NewReader(body))
	if err != nil {
		return service.MenuSyncResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Venue-API-Key", c.venueAPIKey)
	var response platformMenuSyncResponse
	if err := c.do(request, &response); err != nil {
		return service.MenuSyncResult{}, err
	}
	mappings := make([]repository.ProductMapping, 0, len(response.Items))
	for _, item := range response.Items {
		mappings = append(mappings, repository.ProductMapping{ExternalItemID: item.ExternalItemID, ProductID: item.ProductID.String()})
	}
	return service.MenuSyncResult{ProductMappings: mappings}, nil
}

func (c *Client) PullOrderEvents(ctx context.Context, cursor string) (service.PartnerOrderEventPage, error) {
	query := url.Values{"limit": []string{"100"}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/partner/v1/order-events?"+query.Encode(), nil)
	if err != nil {
		return service.PartnerOrderEventPage{}, err
	}
	request.Header.Set("X-Venue-API-Key", c.venueAPIKey)
	var page platformOrderEventPage
	if err := c.do(request, &page); err != nil {
		return service.PartnerOrderEventPage{}, err
	}
	events := make([]service.PartnerOrderEvent, 0, len(page.Items))
	for _, event := range page.Items {
		events = append(events, service.PartnerOrderEvent{ID: event.EventID.String(), OccurredAt: event.OccurredAt, Order: toPartnerOrder(event.Order)})
	}
	return service.PartnerOrderEventPage{Items: events, NextCursor: page.NextCursor}, nil
}

func (c *Client) AcceptOrder(ctx context.Context, order venueapi.VenueOrder) error {
	payload := struct {
		EstimatedPreparationMinutes int32  `json:"estimatedPreparationMinutes"`
		VenueOrderID                string `json:"venueOrderId"`
	}{EstimatedPreparationMinutes: 20, VenueOrderID: order.VenueOrderId}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/partner/v1/orders/%s/accept", c.baseURL, order.PlatformOrderId), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Venue-API-Key", c.venueAPIKey)
	request.Header.Set("Idempotency-Key", "venue-accept-"+order.PlatformOrderId.String())
	return c.do(request, nil)
}

func (c *Client) RejectOrder(ctx context.Context, order venueapi.VenueOrder, reason *string, idempotencyKey string) error {
	payload := struct {
		Reason  string  `json:"reason"`
		Comment *string `json:"comment,omitempty"`
	}{Reason: "other", Comment: reason}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/partner/v1/orders/%s/reject", c.baseURL, order.PlatformOrderId), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Venue-API-Key", c.venueAPIKey)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	return c.do(request, nil)
}

func (c *Client) PushStatus(ctx context.Context, order venueapi.VenueOrder, idempotencyKey string) error {
	payload := struct {
		Status venueapi.VenueOrderStatus `json:"status"`
	}{Status: order.Status}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/partner/v1/orders/%s/status", c.baseURL, order.PlatformOrderId), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Venue-API-Key", c.venueAPIKey)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	return c.do(request, nil)
}

func (c *Client) do(request *http.Request, target any) error {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var problem venueapi.ProblemDetails
		_ = json.NewDecoder(response.Body).Decode(&problem)
		return &service.PlatformError{StatusCode: response.StatusCode, Code: problem.Code, Message: problem.Message}
	}
	if target == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(target)
}

type platformOrderEventPage struct {
	Items      []platformOrderEvent `json:"items"`
	NextCursor *string              `json:"nextCursor"`
}

type platformMenuSyncResponse struct {
	Items []platformMenuItemMapping `json:"items"`
}

type platformMenuItemMapping struct {
	ExternalItemID string    `json:"externalItemId"`
	ProductID      uuid.UUID `json:"productId"`
}

type platformOrderEvent struct {
	EventID    uuid.UUID     `json:"eventId"`
	OccurredAt time.Time     `json:"occurredAt"`
	Order      platformOrder `json:"order"`
}

type platformOrder struct {
	Id              uuid.UUID           `json:"id"`
	Status          string              `json:"status"`
	Items           []platformOrderItem `json:"items"`
	Total           venueapi.Money      `json:"total"`
	RejectionReason *string             `json:"rejectionReason"`
	CreatedAt       time.Time           `json:"createdAt"`
	UpdatedAt       time.Time           `json:"updatedAt"`
}

type platformOrderItem struct {
	ProductId string         `json:"productId"`
	Name      string         `json:"name"`
	Quantity  int32          `json:"quantity"`
	UnitPrice venueapi.Money `json:"unitPrice"`
}

func toPartnerOrder(order platformOrder) service.PartnerOrder {
	items := make([]service.PartnerOrderItem, 0, len(order.Items))
	for _, item := range order.Items {
		items = append(items, service.PartnerOrderItem{ProductID: item.ProductId, Name: item.Name, Quantity: item.Quantity, UnitPrice: item.UnitPrice})
	}
	return service.PartnerOrder{PlatformOrderID: order.Id, Status: venueapi.VenueOrderStatus(order.Status), Items: items, Total: order.Total, RejectionReason: order.RejectionReason, CreatedAt: order.CreatedAt, UpdatedAt: order.UpdatedAt}
}
