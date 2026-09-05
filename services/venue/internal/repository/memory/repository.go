// Package memory is the in-memory venue repository for the MVP demo service.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	venueapi "mealroute/venue/internal/api/venue"
	"mealroute/venue/internal/repository"
)

type Repository struct {
	mu       sync.RWMutex
	menu     venueapi.VenueMenu
	orders   map[string]venueapi.VenueOrder
	products map[string]string
	orderID  []string
	cursor   string
	events   map[string]struct{}
}

func New() *Repository {
	sortZero := int32(0)
	now := time.Now().UTC()
	return &Repository{
		menu: venueapi.VenueMenu{
			VenueExternalId: "demo-pizza-1",
			MenuVersion:     2,
			Currency:        venueapi.VenueMenuCurrencyRUB,
			UpdatedAt:       now,
			Categories: []venueapi.VenueMenuCategory{{
				ExternalCategoryId: "pizza",
				Name:               "Пицца",
				SortOrder:          &sortZero,
				Items: []venueapi.VenueMenuItem{
					{ExternalItemId: "pizza-pepperoni", Name: "Пепперони", Description: stringPtr("Острая салями, моцарелла и томатный соус"), Price: venueapi.Money{Amount: 54900, Currency: venueapi.MoneyCurrencyRUB}, Available: true, SortOrder: &sortZero},
					{ExternalItemId: "pizza-margherita", Name: "Маргарита", Description: stringPtr("Моцарелла, томаты и базилик"), Price: venueapi.Money{Amount: 44900, Currency: venueapi.MoneyCurrencyRUB}, Available: true, SortOrder: &sortZero},
				},
			}},
		},
		orders:   make(map[string]venueapi.VenueOrder),
		products: make(map[string]string),
		events:   make(map[string]struct{}),
	}
}

func (r *Repository) Menu(_ context.Context) venueapi.VenueMenu {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.menu
}

func (r *Repository) ListOrders(_ context.Context, status string, cursor *repository.OrderCursor, limit int32) []venueapi.VenueOrder {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]venueapi.VenueOrder, 0, len(r.orderID))
	for _, orderID := range r.orderID {
		order := r.orders[orderID]
		if status == "" || string(order.Status) == status {
			if cursor != nil && (order.CreatedAt.After(cursor.CreatedAt) || (order.CreatedAt.Equal(cursor.CreatedAt) && order.VenueOrderId >= cursor.VenueOrderID)) {
				continue
			}
			items = append(items, order)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].VenueOrderId > items[j].VenueOrderId
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	if int32(len(items)) > limit {
		items = items[:limit]
	}
	return items
}

func (r *Repository) FindOrder(_ context.Context, venueOrderID string) (venueapi.VenueOrder, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	order, ok := r.orders[venueOrderID]
	return order, ok
}

func (r *Repository) SaveOrder(_ context.Context, order venueapi.VenueOrder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.orders[order.VenueOrderId]; !exists {
		r.orderID = append(r.orderID, order.VenueOrderId)
	}
	r.orders[order.VenueOrderId] = order
}

func (r *Repository) SaveProductMappings(_ context.Context, mappings []repository.ProductMapping) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, mapping := range mappings {
		r.products[mapping.ProductID] = mapping.ExternalItemID
	}
}

func (r *Repository) ExternalItemID(_ context.Context, productID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	externalItemID, ok := r.products[productID]
	return externalItemID, ok
}

func (r *Repository) PartnerCursor(_ context.Context) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cursor
}

func (r *Repository) SavePartnerCursor(_ context.Context, cursor string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursor = cursor
}

func (r *Repository) IsEventProcessed(_ context.Context, eventID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, processed := r.events[eventID]
	return processed
}

func (r *Repository) MarkEventProcessed(_ context.Context, eventID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events[eventID] = struct{}{}
}

func stringPtr(value string) *string {
	return &value
}

var _ repository.Repository = (*Repository)(nil)
