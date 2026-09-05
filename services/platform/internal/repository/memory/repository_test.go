package memory

import (
	"context"
	"testing"
	"time"

	platformapi "mealroute/platform/internal/api/platform"
	repositorypkg "mealroute/platform/internal/repository"

	"github.com/google/uuid"
)

func TestPartnerDataIsScopedToVenue(t *testing.T) {
	ctx := context.Background()
	repository := New()
	demoVenueID := uuid.MustParse(DemoVenueID)
	secondVenueID := uuid.MustParse("00000000-0000-0000-0000-000000000101")
	demoOrderID := uuid.MustParse("00000000-0000-0000-0000-000000000102")
	secondOrderID := uuid.MustParse("00000000-0000-0000-0000-000000000103")
	now := time.Now().UTC()

	demoOrder := platformapi.Order{Id: demoOrderID, VenueId: demoVenueID, Status: platformapi.OrderStatusPendingConfirmation, CreatedAt: now, UpdatedAt: now}
	secondOrder := platformapi.Order{Id: secondOrderID, VenueId: secondVenueID, Status: platformapi.OrderStatusPendingConfirmation, CreatedAt: now, UpdatedAt: now}

	repository.mu.Lock()
	repository.venues[secondVenueID] = platformapi.Venue{Id: secondVenueID, Name: "Второе заведение", Status: platformapi.Active}
	repository.venueOrder = append(repository.venueOrder, secondVenueID)
	repository.venueAPIKeys["second-venue-key"] = secondVenueID
	repository.orders[demoOrderID] = demoOrder
	repository.orders[secondOrderID] = secondOrder
	repository.orderOrder = []uuid.UUID{demoOrderID, secondOrderID}
	repository.events = []eventRecord{
		{sequence: 1, event: platformapi.OrderEvent{EventId: uuid.New(), Type: platformapi.OrderEventTypeCreated, OccurredAt: now, Order: demoOrder}},
		{sequence: 2, event: platformapi.OrderEvent{EventId: uuid.New(), Type: platformapi.OrderEventTypeCreated, OccurredAt: now, Order: secondOrder}},
	}
	repository.mu.Unlock()

	venueID, found, err := repository.FindVenueIDByAPIKey(ctx, "second-venue-key")
	if err != nil || !found || venueID != secondVenueID {
		t.Fatalf("API key must resolve to the second venue: id=%s found=%t err=%v", venueID, found, err)
	}

	orders, err := repository.ListOrdersForPartner(ctx, demoVenueID, "", nil, 20)
	if err != nil {
		t.Fatalf("list partner orders: %v", err)
	}
	if len(orders) != 1 || orders[0].Id != demoOrderID {
		t.Fatalf("demo venue received foreign orders: %+v", orders)
	}

	if _, found, err := repository.FindOrderForVenue(ctx, demoVenueID, secondOrderID); err != nil || found {
		t.Fatalf("demo venue must not find second venue order: found=%t err=%v", found, err)
	}

	events, _, _, err := repository.ListOrderEvents(ctx, demoVenueID, 0, 20)
	if err != nil {
		t.Fatalf("list partner events: %v", err)
	}
	if len(events) != 1 || events[0].Order.Id != demoOrderID {
		t.Fatalf("demo venue received foreign events: %+v", events)
	}
}

func TestVenueKeysetPagination(t *testing.T) {
	ctx := context.Background()
	repository := New()
	now := time.Now().UTC()
	additional := []platformapi.Venue{
		{Id: uuid.MustParse("00000000-0000-0000-0000-000000000201"), Name: "Альфа", City: "Новосибирск", Status: platformapi.Active, UpdatedAt: now},
		{Id: uuid.MustParse("00000000-0000-0000-0000-000000000202"), Name: "Бета", City: "Новосибирск", Status: platformapi.Active, UpdatedAt: now},
		{Id: uuid.MustParse("00000000-0000-0000-0000-000000000203"), Name: "Яблоко", City: "Новосибирск", Status: platformapi.Active, UpdatedAt: now},
	}
	repository.mu.Lock()
	for _, venue := range additional {
		repository.venues[venue.Id] = venue
		repository.venueOrder = append(repository.venueOrder, venue.Id)
	}
	repository.mu.Unlock()

	first, err := repository.ListVenues(ctx, "Новосибирск", nil, 2)
	if err != nil {
		t.Fatalf("list first venue page: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("expected two venues, got %d", len(first))
	}
	cursor := &repositorypkg.VenueCursor{Name: first[1].Name, ID: first[1].Id}
	second, err := repository.ListVenues(ctx, "Новосибирск", cursor, 2)
	if err != nil {
		t.Fatalf("list second venue page: %v", err)
	}
	if len(second) != 2 || second[0].Id == first[0].Id || second[0].Id == first[1].Id {
		t.Fatalf("unexpected second venue page: %+v", second)
	}
}
