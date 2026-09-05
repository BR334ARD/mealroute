package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	venueapi "mealroute/venue/internal/api/venue"
	"mealroute/venue/internal/repository/memory"
	"mealroute/venue/internal/service"

	"github.com/google/uuid"
)

func TestOrderStatusMovesForwardOnly(t *testing.T) {
	ctx := context.Background()
	repository := memory.New()
	application := service.New(repository, nil)
	platformOrderID := uuid.New()
	venueOrderID := "venue-" + platformOrderID.String()
	repository.SaveOrder(ctx, venueapi.VenueOrder{
		VenueOrderId:    venueOrderID,
		PlatformOrderId: platformOrderID,
		Status:          venueapi.VenueOrderStatusAccepted,
		Items:           []venueapi.VenueOrderItem{{ExternalItemId: "pizza-pepperoni", Name: "Пепперони", Quantity: 1, UnitPrice: venueapi.Money{Amount: 54900, Currency: venueapi.MoneyCurrencyRUB}}},
		Total:           venueapi.Money{Amount: 74800, Currency: venueapi.MoneyCurrencyRUB},
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	})
	if _, domainError := application.UpdateOrderStatus(ctx, venueOrderID, venueapi.VenueCommandStatusPreparing, nil); domainError != nil {
		t.Fatalf("advance order: %v", domainError)
	}
	if _, domainError := application.UpdateOrderStatus(ctx, venueOrderID, venueapi.VenueCommandStatusAccepted, nil); domainError == nil {
		t.Fatal("expected backward status transition to fail")
	}
}

func TestStatusIsStoredOnlyAfterPlatformConfirmsIt(t *testing.T) {
	ctx := context.Background()
	repository := memory.New()
	gateway := &fakePlatformGateway{pushErr: errors.New("temporary network failure")}
	application := service.New(repository, gateway)
	platformOrderID := uuid.New()
	stored := testVenueOrder(platformOrderID, venueapi.VenueOrderStatusAccepted, time.Now().UTC())
	repository.SaveOrder(ctx, stored)

	if _, domainError := application.UpdateOrderStatus(ctx, stored.VenueOrderId, venueapi.VenueCommandStatusPreparing, nil); domainError == nil || domainError.Code != "platform_unavailable" {
		t.Fatalf("expected platform_unavailable, got %v", domainError)
	}
	afterFailure, found := repository.FindOrder(ctx, stored.VenueOrderId)
	if !found || afterFailure.Status != venueapi.VenueOrderStatusAccepted {
		t.Fatalf("failed push changed local state: %+v", afterFailure)
	}

	gateway.pushErr = nil
	updated, domainError := application.UpdateOrderStatus(ctx, stored.VenueOrderId, venueapi.VenueCommandStatusPreparing, nil)
	if domainError != nil || updated.Status != venueapi.VenueOrderStatusPreparing {
		t.Fatalf("retry status update: order=%+v err=%v", updated, domainError)
	}
	if len(gateway.pushed) != 1 || len(gateway.statusKeys) != 1 || gateway.statusKeys[0] != "venue-status-"+stored.VenueOrderId+"-preparing" {
		t.Fatalf("unexpected reliable push: orders=%+v keys=%+v", gateway.pushed, gateway.statusKeys)
	}
}

func TestOperatorRejectsPendingOrderAfterPlatformConfirms(t *testing.T) {
	ctx := context.Background()
	repository := memory.New()
	gateway := &fakePlatformGateway{}
	application := service.New(repository, gateway)
	stored := testVenueOrder(uuid.New(), venueapi.VenueOrderStatusPendingConfirmation, time.Now().UTC())
	repository.SaveOrder(ctx, stored)
	reason := "kitchen capacity exhausted"

	updated, domainError := application.UpdateOrderStatus(ctx, stored.VenueOrderId, venueapi.VenueCommandStatusRejected, &reason)
	if domainError != nil || updated.Status != venueapi.VenueOrderStatusRejected {
		t.Fatalf("reject order: order=%+v err=%v", updated, domainError)
	}
	if len(gateway.rejected) != 1 || len(gateway.statusKeys) != 1 || gateway.statusKeys[0] != "venue-status-"+stored.VenueOrderId+"-rejected" {
		t.Fatalf("reject command was not delivered reliably: rejected=%+v keys=%+v", gateway.rejected, gateway.statusKeys)
	}
}

func TestReasonIsRejectedForNonRejectedStatus(t *testing.T) {
	ctx := context.Background()
	repository := memory.New()
	gateway := &fakePlatformGateway{}
	application := service.New(repository, gateway)
	stored := testVenueOrder(uuid.New(), venueapi.VenueOrderStatusAccepted, time.Now().UTC())
	repository.SaveOrder(ctx, stored)
	reason := "must not leak into preparing"

	_, domainError := application.UpdateOrderStatus(ctx, stored.VenueOrderId, venueapi.VenueCommandStatusPreparing, &reason)
	if domainError == nil || domainError.Code != "invalid_request" {
		t.Fatalf("expected reason validation error, got %v", domainError)
	}
	unchanged, found := repository.FindOrder(ctx, stored.VenueOrderId)
	if !found || unchanged.Status != venueapi.VenueOrderStatusAccepted || unchanged.RejectionReason != nil || len(gateway.pushed) != 0 {
		t.Fatalf("invalid reason temporarily changed the order: %+v", unchanged)
	}
}

func TestVenueOrderPaginationUsesCursorAndStatusScope(t *testing.T) {
	ctx := context.Background()
	repository := memory.New()
	application := service.New(repository, nil)
	baseTime := time.Now().UTC()
	for index := 0; index < 3; index++ {
		order := testVenueOrder(uuid.New(), venueapi.VenueOrderStatusAccepted, baseTime.Add(time.Duration(index)*time.Second))
		repository.SaveOrder(ctx, order)
	}

	first, domainError := application.ListOrders(ctx, string(venueapi.VenueOrderStatusAccepted), "", 2)
	if domainError != nil || len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("unexpected first page: page=%+v err=%v", first, domainError)
	}
	second, domainError := application.ListOrders(ctx, string(venueapi.VenueOrderStatusAccepted), *first.NextCursor, 2)
	if domainError != nil || len(second.Items) != 1 || second.NextCursor != nil {
		t.Fatalf("unexpected second page: page=%+v err=%v", second, domainError)
	}
	seen := map[uuid.UUID]struct{}{}
	for _, order := range append(first.Items, second.Items...) {
		seen[order.PlatformOrderId] = struct{}{}
	}
	if len(seen) != 3 {
		t.Fatalf("pagination returned duplicates: %+v", seen)
	}
	if _, domainError := application.ListOrders(ctx, string(venueapi.VenueOrderStatusPreparing), *first.NextCursor, 2); domainError == nil || domainError.Code != "invalid_request" {
		t.Fatalf("expected cursor scope validation, got %v", domainError)
	}
}

func testVenueOrder(platformOrderID uuid.UUID, status venueapi.VenueOrderStatus, createdAt time.Time) venueapi.VenueOrder {
	return venueapi.VenueOrder{
		VenueOrderId:    "venue-" + platformOrderID.String(),
		PlatformOrderId: platformOrderID,
		Status:          status,
		Items:           []venueapi.VenueOrderItem{{ExternalItemId: "pizza-pepperoni", Name: "Пепперони", Quantity: 1, UnitPrice: venueapi.Money{Amount: 54900, Currency: venueapi.MoneyCurrencyRUB}}},
		Total:           venueapi.Money{Amount: 74800, Currency: venueapi.MoneyCurrencyRUB},
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	}
}
