package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"

	venueapi "mealroute/venue/internal/api/venue"
	"mealroute/venue/internal/domain"
	"mealroute/venue/internal/repository"
)

const venueOrdersCursorKind = "venue-orders-v1"

type orderPageCursor struct {
	Version      int       `json:"v"`
	Kind         string    `json:"kind"`
	Scope        string    `json:"scope"`
	CreatedAt    time.Time `json:"createdAt"`
	VenueOrderID string    `json:"venueOrderId"`
}

func encodeOrderCursor(status string, order venueapi.VenueOrder) string {
	payload, _ := json.Marshal(orderPageCursor{
		Version:      1,
		Kind:         venueOrdersCursorKind,
		Scope:        orderCursorScope(status),
		CreatedAt:    order.CreatedAt,
		VenueOrderID: order.VenueOrderId,
	})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeOrderCursor(raw, status string) (*repository.OrderCursor, *domain.Error) {
	if raw == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, invalidCursorError()
	}
	var cursor orderPageCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return nil, invalidCursorError()
	}
	if cursor.Version != 1 || cursor.Kind != venueOrdersCursorKind || cursor.Scope != orderCursorScope(status) || cursor.CreatedAt.IsZero() || cursor.VenueOrderID == "" {
		return nil, invalidCursorError()
	}
	return &repository.OrderCursor{CreatedAt: cursor.CreatedAt, VenueOrderID: cursor.VenueOrderID}, nil
}

func orderCursorScope(status string) string {
	sum := sha256.Sum256([]byte(status))
	return hex.EncodeToString(sum[:])
}

func invalidCursorError() *domain.Error {
	return domain.NewError("invalid_request", "cursor is invalid for this request")
}
