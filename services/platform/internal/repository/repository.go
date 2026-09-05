// Package repository defines persistence ports for platform use cases.
package repository

import (
	"context"
	"time"

	platformapi "mealroute/platform/internal/api/platform"

	"github.com/google/uuid"
)

type VenueCursor struct {
	Name string
	ID   uuid.UUID
}

type OrderCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// OrderCommand groups every durable effect of a state-changing order command.
// Its order snapshot, event and idempotency record must be committed together.
type OrderCommand struct {
	Subject        string
	Operation      string
	IdempotencyKey string
	RequestHash    string
	MenuVersion    int64
	ExpectedStatus platformapi.OrderStatus
	Order          platformapi.Order
	Event          platformapi.OrderEvent
	VenueOrderID   *string
}

type OrderCommandResult struct {
	Order               platformapi.Order
	RequestHashMismatch bool
	MenuVersionConflict bool
	StateConflict       bool
}

type Repository interface {
	FindVenueIDByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, bool, error)
	ListVenues(ctx context.Context, city string, cursor *VenueCursor, limit int32) ([]platformapi.Venue, error)
	FindVenue(ctx context.Context, id uuid.UUID) (platformapi.Venue, bool, error)
	FindMenu(ctx context.Context, venueID uuid.UUID) (platformapi.Menu, bool, error)
	FindOrderForUser(ctx context.Context, userID string, orderID uuid.UUID) (platformapi.Order, bool, error)
	FindOrderForVenue(ctx context.Context, venueID, orderID uuid.UUID) (platformapi.Order, bool, error)
	ListOrdersForUser(ctx context.Context, userID string, cursor *OrderCursor, limit int32) ([]platformapi.Order, error)
	ListOrdersForPartner(ctx context.Context, venueID uuid.UUID, status string, cursor *OrderCursor, limit int32) ([]platformapi.Order, error)
	CreateOrderCommand(ctx context.Context, userID string, command OrderCommand) (OrderCommandResult, error)
	ApplyOrderCommand(ctx context.Context, command OrderCommand) (OrderCommandResult, error)
	VenueOrderID(ctx context.Context, orderID uuid.UUID) (string, bool, error)
	FindCommand(ctx context.Context, subject, operation, key string) (order platformapi.Order, requestHash string, found bool, err error)
	ListOrderEvents(ctx context.Context, venueID uuid.UUID, afterSequence uint64, limit int32) (events []platformapi.OrderEvent, lastSequence uint64, hasEvents bool, err error)
	ProductMappings(ctx context.Context, venueID uuid.UUID) (map[string]uuid.UUID, error)
	SaveMenuSnapshot(ctx context.Context, venue platformapi.Venue, menu platformapi.Menu, productIDs map[string]uuid.UUID) (applied bool, err error)
}
