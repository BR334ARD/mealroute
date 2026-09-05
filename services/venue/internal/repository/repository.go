// Package repository defines storage ports used by venue use cases.
package repository

import (
	"context"
	"time"

	venueapi "mealroute/venue/internal/api/venue"
)

// ProductMapping is the stable correspondence between a local venue item and
// the public product ID assigned by platform during menu synchronization.
type ProductMapping struct {
	ExternalItemID string
	ProductID      string
}

type OrderCursor struct {
	CreatedAt    time.Time
	VenueOrderID string
}

type Repository interface {
	Menu(ctx context.Context) venueapi.VenueMenu
	ListOrders(ctx context.Context, status string, cursor *OrderCursor, limit int32) []venueapi.VenueOrder
	FindOrder(ctx context.Context, venueOrderID string) (venueapi.VenueOrder, bool)
	SaveOrder(ctx context.Context, order venueapi.VenueOrder)
	SaveProductMappings(ctx context.Context, mappings []ProductMapping)
	ExternalItemID(ctx context.Context, productID string) (string, bool)
	PartnerCursor(ctx context.Context) string
	SavePartnerCursor(ctx context.Context, cursor string)
	IsEventProcessed(ctx context.Context, eventID string) bool
	MarkEventProcessed(ctx context.Context, eventID string)
}
