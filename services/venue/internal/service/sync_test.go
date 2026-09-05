package service_test

import (
	"context"
	"testing"
	"time"

	venueapi "mealroute/venue/internal/api/venue"
	"mealroute/venue/internal/repository"
	"mealroute/venue/internal/repository/memory"
	"mealroute/venue/internal/service"

	"github.com/google/uuid"
)

type fakePlatformGateway struct {
	page       service.PartnerOrderEventPage
	pages      []service.PartnerOrderEventPage
	pullIndex  int
	menuSync   service.MenuSyncResult
	menuErr    error
	acceptErr  error
	rejectErr  error
	pushErr    error
	accepted   []venueapi.VenueOrder
	rejected   []venueapi.VenueOrder
	pushed     []venueapi.VenueOrder
	statusKeys []string
}

func (f *fakePlatformGateway) SyncMenu(context.Context, venueapi.VenueMenu) (service.MenuSyncResult, error) {
	return f.menuSync, f.menuErr
}

func (f *fakePlatformGateway) PullOrderEvents(context.Context, string) (service.PartnerOrderEventPage, error) {
	if f.pullIndex < len(f.pages) {
		page := f.pages[f.pullIndex]
		f.pullIndex++
		return page, nil
	}
	return f.page, nil
}

func (f *fakePlatformGateway) AcceptOrder(_ context.Context, order venueapi.VenueOrder) error {
	if f.acceptErr != nil {
		return f.acceptErr
	}
	f.accepted = append(f.accepted, order)
	return nil
}

func (f *fakePlatformGateway) RejectOrder(_ context.Context, order venueapi.VenueOrder, _ *string, key string) error {
	if f.rejectErr != nil {
		return f.rejectErr
	}
	f.rejected = append(f.rejected, order)
	f.statusKeys = append(f.statusKeys, key)
	return nil
}

func (f *fakePlatformGateway) PushStatus(_ context.Context, order venueapi.VenueOrder, key string) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed = append(f.pushed, order)
	f.statusKeys = append(f.statusKeys, key)
	return nil
}

func TestSyncStoresNewPendingOrderAndPersistsCursor(t *testing.T) {
	ctx := context.Background()
	platformOrderID := uuid.New()
	nextCursor := "1"
	gateway := &fakePlatformGateway{menuSync: service.MenuSyncResult{ProductMappings: []repository.ProductMapping{{ExternalItemID: "pizza-pepperoni", ProductID: "platform-pepperoni"}}}, page: service.PartnerOrderEventPage{
		Items: []service.PartnerOrderEvent{{
			ID: uuid.NewString(),
			Order: service.PartnerOrder{
				PlatformOrderID: platformOrderID,
				Status:          venueapi.VenueOrderStatusPendingConfirmation,
				Items:           []service.PartnerOrderItem{{ProductID: "platform-pepperoni", Name: "Пепперони", Quantity: 1, UnitPrice: venueapi.Money{Amount: 54900, Currency: venueapi.MoneyCurrencyRUB}}},
				Total:           venueapi.Money{Amount: 74800, Currency: venueapi.MoneyCurrencyRUB},
				CreatedAt:       time.Now().UTC(),
				UpdatedAt:       time.Now().UTC(),
			},
		}},
		NextCursor: &nextCursor,
	}}
	repository := memory.New()
	application := service.New(repository, gateway)

	if err := application.SyncWithPlatform(ctx); err != nil {
		t.Fatalf("sync with platform: %v", err)
	}
	if len(gateway.accepted) != 0 || len(gateway.rejected) != 0 {
		t.Fatalf("sync worker must not decide for the operator: accepted=%d rejected=%d", len(gateway.accepted), len(gateway.rejected))
	}
	order, domainError := application.GetOrder(ctx, expectedVenueOrderID(platformOrderID))
	if domainError != nil {
		t.Fatalf("get local order: %v", domainError)
	}
	if order.Status != venueapi.VenueOrderStatusPendingConfirmation {
		t.Fatalf("expected pending local order, got %s", order.Status)
	}
	if order.VenueOrderId == order.PlatformOrderId.String() {
		t.Fatal("venue and platform order IDs must belong to different namespaces")
	}
	if order.Items[0].ExternalItemId != "pizza-pepperoni" {
		t.Fatalf("expected local external item ID, got %q", order.Items[0].ExternalItemId)
	}
	if repository.PartnerCursor(ctx) != nextCursor {
		t.Fatalf("expected cursor %q, got %q", nextCursor, repository.PartnerCursor(ctx))
	}
}

func TestSyncContinuesPastFailedEventAndRetriesIt(t *testing.T) {
	ctx := context.Background()
	failedOrderID := uuid.New()
	successfulOrderID := uuid.New()
	now := time.Now().UTC()
	nextCursor := "2"
	gateway := &fakePlatformGateway{
		menuSync: service.MenuSyncResult{ProductMappings: []repository.ProductMapping{{ExternalItemID: "pizza-pepperoni", ProductID: "known-product"}}},
		pages: []service.PartnerOrderEventPage{{
			Items: []service.PartnerOrderEvent{
				newCreatedEvent(failedOrderID, "missing-product", now),
				newCreatedEvent(successfulOrderID, "known-product", now.Add(time.Second)),
			},
			NextCursor: &nextCursor,
		}, {}},
	}
	repository := memory.New()
	application := service.New(repository, gateway)

	if err := application.SyncWithPlatform(ctx); err == nil {
		t.Fatal("expected the failed event to be reported")
	}
	if repository.PartnerCursor(ctx) != nextCursor {
		t.Fatalf("journal cursor was blocked: %q", repository.PartnerCursor(ctx))
	}
	if _, domainError := application.GetOrder(ctx, expectedVenueOrderID(successfulOrderID)); domainError != nil {
		t.Fatalf("later event was blocked: %v", domainError)
	}

	gateway.menuSync.ProductMappings = append(gateway.menuSync.ProductMappings, recoveredMapping("missing-product"))
	if err := application.SyncWithPlatform(ctx); err != nil {
		t.Fatalf("retry pending event: %v", err)
	}
	if _, domainError := application.GetOrder(ctx, expectedVenueOrderID(failedOrderID)); domainError != nil {
		t.Fatalf("failed event was not retried: %v", domainError)
	}
}

func TestStaleCreatedSnapshotDoesNotBlockNewerCancellation(t *testing.T) {
	ctx := context.Background()
	orderID := uuid.New()
	now := time.Now().UTC()
	nextCursor := "2"
	cancelled := newCreatedEvent(orderID, "known-product", now.Add(time.Second))
	cancelled.ID = uuid.NewString()
	cancelled.Order.Status = venueapi.VenueOrderStatusCancelled
	cancelled.Order.UpdatedAt = now.Add(time.Second)
	gateway := &fakePlatformGateway{
		menuSync: service.MenuSyncResult{ProductMappings: []repository.ProductMapping{{ExternalItemID: "pizza-pepperoni", ProductID: "known-product"}}},
		page: service.PartnerOrderEventPage{
			Items:      []service.PartnerOrderEvent{newCreatedEvent(orderID, "known-product", now), cancelled},
			NextCursor: &nextCursor,
		},
	}
	repository := memory.New()
	application := service.New(repository, gateway)

	if err := application.SyncWithPlatform(ctx); err != nil {
		t.Fatalf("stale created snapshot must be non-fatal: %v", err)
	}
	order, domainError := application.GetOrder(ctx, expectedVenueOrderID(orderID))
	if domainError != nil || order.Status != venueapi.VenueOrderStatusCancelled {
		t.Fatalf("newer cancellation was not applied: order=%+v err=%v", order, domainError)
	}
	if repository.PartnerCursor(ctx) != nextCursor {
		t.Fatalf("expected cursor %q, got %q", nextCursor, repository.PartnerCursor(ctx))
	}
}

func TestPendingCreatedSnapshotDoesNotHideLaterPlatformCancellation(t *testing.T) {
	ctx := context.Background()
	orderID := uuid.New()
	platformCreatedAt := time.Unix(1_700_000_000, 0).UTC()
	firstCursor := "1"
	secondCursor := "2"
	cancelled := newCreatedEvent(orderID, "known-product", platformCreatedAt.Add(time.Second))
	cancelled.ID = uuid.NewString()
	cancelled.Order.Status = venueapi.VenueOrderStatusCancelled
	gateway := &fakePlatformGateway{
		menuSync: service.MenuSyncResult{ProductMappings: []repository.ProductMapping{{ExternalItemID: "pizza-pepperoni", ProductID: "known-product"}}},
		pages: []service.PartnerOrderEventPage{
			{Items: []service.PartnerOrderEvent{newCreatedEvent(orderID, "known-product", platformCreatedAt)}, NextCursor: &firstCursor},
			{Items: []service.PartnerOrderEvent{cancelled}, NextCursor: &secondCursor},
		},
	}
	repository := memory.New()
	application := service.New(repository, gateway)

	if err := application.SyncWithPlatform(ctx); err != nil {
		t.Fatalf("store created order: %v", err)
	}
	if err := application.SyncWithPlatform(ctx); err != nil {
		t.Fatalf("apply later cancellation: %v", err)
	}
	order, domainError := application.GetOrder(ctx, expectedVenueOrderID(orderID))
	if domainError != nil || order.Status != venueapi.VenueOrderStatusCancelled {
		t.Fatalf("platform cancellation was hidden by a local timestamp: order=%+v err=%v", order, domainError)
	}
}

func TestSyncPreservesRejectionReasonFromCanonicalEvent(t *testing.T) {
	ctx := context.Background()
	orderID := uuid.New()
	reason := "kitchen capacity exhausted"
	rejected := newCreatedEvent(orderID, "known-product", time.Now().UTC())
	rejected.Order.Status = venueapi.VenueOrderStatusRejected
	rejected.Order.RejectionReason = &reason
	gateway := &fakePlatformGateway{
		menuSync: service.MenuSyncResult{ProductMappings: []repository.ProductMapping{{ExternalItemID: "pizza-pepperoni", ProductID: "known-product"}}},
		page:     service.PartnerOrderEventPage{Items: []service.PartnerOrderEvent{rejected}},
	}
	application := service.New(memory.New(), gateway)

	if err := application.SyncWithPlatform(ctx); err != nil {
		t.Fatalf("apply rejected event: %v", err)
	}
	order, domainError := application.GetOrder(ctx, expectedVenueOrderID(orderID))
	if domainError != nil {
		t.Fatalf("get rejected order: %v", domainError)
	}
	if order.Status != venueapi.VenueOrderStatusRejected || order.RejectionReason == nil || *order.RejectionReason != reason {
		t.Fatalf("canonical rejection reason was not preserved: %+v", order)
	}
}

func TestVenueOrderIDIsStableAcrossJournalReplay(t *testing.T) {
	ctx := context.Background()
	orderID := uuid.New()
	event := newCreatedEvent(orderID, "known-product", time.Now().UTC())
	mapping := service.MenuSyncResult{ProductMappings: []repository.ProductMapping{{ExternalItemID: "pizza-pepperoni", ProductID: "known-product"}}}
	ids := make([]string, 0, 2)
	for replay := 0; replay < 2; replay++ {
		gateway := &fakePlatformGateway{menuSync: mapping, page: service.PartnerOrderEventPage{Items: []service.PartnerOrderEvent{event}}}
		application := service.New(memory.New(), gateway)
		if err := application.SyncWithPlatform(ctx); err != nil {
			t.Fatalf("replay %d: %v", replay, err)
		}
		order, domainError := application.GetOrder(ctx, expectedVenueOrderID(orderID))
		if domainError != nil {
			t.Fatalf("get replayed order %d: %v", replay, domainError)
		}
		ids = append(ids, order.VenueOrderId)
	}
	if ids[0] != ids[1] || ids[0] == orderID.String() {
		t.Fatalf("venue order ID is not stable and separate: platform=%s venue=%v", orderID, ids)
	}
}

func newCreatedEvent(orderID uuid.UUID, productID string, occurredAt time.Time) service.PartnerOrderEvent {
	return service.PartnerOrderEvent{
		ID:         uuid.NewString(),
		OccurredAt: occurredAt,
		Order: service.PartnerOrder{
			PlatformOrderID: orderID,
			Status:          venueapi.VenueOrderStatusPendingConfirmation,
			Items:           []service.PartnerOrderItem{{ProductID: productID, Name: "Пепперони", Quantity: 1, UnitPrice: venueapi.Money{Amount: 54900, Currency: venueapi.MoneyCurrencyRUB}}},
			Total:           venueapi.Money{Amount: 74800, Currency: venueapi.MoneyCurrencyRUB},
			CreatedAt:       occurredAt,
			UpdatedAt:       occurredAt,
		},
	}
}

func recoveredMapping(productID string) repository.ProductMapping {
	return repository.ProductMapping{ExternalItemID: "recovered-item", ProductID: productID}
}

func expectedVenueOrderID(platformOrderID uuid.UUID) string {
	derived := uuid.NewSHA1(uuid.NameSpaceURL, []byte("mealroute:venue-order:"+platformOrderID.String()))
	return "venue-" + derived.String()
}
