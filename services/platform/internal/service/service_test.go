package service_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/domain"
	"mealroute/platform/internal/repository/memory"
	"mealroute/platform/internal/service"

	"github.com/google/uuid"
)

func TestCreateOrderIsIdempotentAndCanBeCancelled(t *testing.T) {
	ctx := context.Background()
	application := service.New(memory.New())
	venueID := uuid.MustParse(memory.DemoVenueID)
	menu, domainError := application.GetMenu(ctx, venueID)
	if domainError != nil {
		t.Fatalf("get menu: %v", domainError)
	}
	itemID := menu.Categories[0].Items[0].Id
	request := platformapi.CreateOrderRequest{
		VenueId:     venueID,
		MenuVersion: menu.Version,
		Items:       []platformapi.OrderItemInput{{ProductId: itemID, Quantity: 1}},
		DeliveryAddress: platformapi.DeliveryAddress{
			City:        "Новосибирск",
			AddressLine: "Красный проспект, 10",
		},
	}

	first, domainError := application.CreateOrder(ctx, "user-1", "key-1", request)
	if domainError != nil {
		t.Fatalf("create order: %v", domainError)
	}
	second, domainError := application.CreateOrder(ctx, "user-1", "key-1", request)
	if domainError != nil {
		t.Fatalf("repeat create order: %v", domainError)
	}
	if first.Id != second.Id {
		t.Fatalf("idempotency returned different ids: %s and %s", first.Id, second.Id)
	}
	if first.Status != platformapi.OrderStatusPendingConfirmation {
		t.Fatalf("unexpected initial status: %s", first.Status)
	}
	changedRequest := request
	changedRequest.Items = []platformapi.OrderItemInput{{ProductId: itemID, Quantity: 2}}
	if _, domainError := application.CreateOrder(ctx, "user-1", "key-1", changedRequest); domainError == nil || domainError.Code != "idempotency_key_reused" {
		t.Fatalf("expected idempotency_key_reused, got %v", domainError)
	}

	cancelled, domainError := application.CancelCustomerOrder(ctx, "user-1", first.Id, "cancel-key-1", nil)
	if domainError != nil {
		t.Fatalf("cancel order: %v", domainError)
	}
	if cancelled.Status != platformapi.OrderStatusCancelled {
		t.Fatalf("unexpected cancelled status: %s", cancelled.Status)
	}

	events, domainError := application.ListPartnerOrderEvents(ctx, venueID, "", 20)
	if domainError != nil {
		t.Fatalf("list order events: %v", domainError)
	}
	if len(events.Items) != 2 {
		t.Fatalf("expected create and cancel events, got %d", len(events.Items))
	}
	if events.Items[0].Type != platformapi.OrderEventTypeCreated || events.Items[1].Type != platformapi.OrderEventTypeCancelled {
		t.Fatalf("unexpected event sequence: %s, %s", events.Items[0].Type, events.Items[1].Type)
	}
}

func TestOrderPaginationReturnsEveryItemAndValidatesCursorScope(t *testing.T) {
	ctx := context.Background()
	application := service.New(memory.New())
	venueID := uuid.MustParse(memory.DemoVenueID)
	menu, domainError := application.GetMenu(ctx, venueID)
	if domainError != nil {
		t.Fatalf("get menu: %v", domainError)
	}
	for index := 0; index < 3; index++ {
		_, domainError := application.CreateOrder(ctx, "pagination-user", fmt.Sprintf("create-%d", index), platformapi.CreateOrderRequest{
			VenueId:     venueID,
			MenuVersion: menu.Version,
			Items:       []platformapi.OrderItemInput{{ProductId: menu.Categories[0].Items[0].Id, Quantity: 1}},
			DeliveryAddress: platformapi.DeliveryAddress{
				City:        "Новосибирск",
				AddressLine: fmt.Sprintf("Страница, %d", index),
			},
		})
		if domainError != nil {
			t.Fatalf("create order %d: %v", index, domainError)
		}
	}

	first, domainError := application.ListCustomerOrders(ctx, "pagination-user", "", 2)
	if domainError != nil {
		t.Fatalf("list first customer page: %v", domainError)
	}
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("expected two items and next cursor, got %+v", first)
	}
	second, domainError := application.ListCustomerOrders(ctx, "pagination-user", *first.NextCursor, 2)
	if domainError != nil {
		t.Fatalf("list second customer page: %v", domainError)
	}
	if len(second.Items) != 1 || second.NextCursor != nil {
		t.Fatalf("expected final one-item page, got %+v", second)
	}
	seen := map[uuid.UUID]struct{}{}
	for _, order := range append(first.Items, second.Items...) {
		seen[order.Id] = struct{}{}
	}
	if len(seen) != 3 {
		t.Fatalf("expected three distinct orders across pages, got %d", len(seen))
	}

	partnerFirst, domainError := application.ListPartnerOrders(ctx, venueID, "", "", 2)
	if domainError != nil {
		t.Fatalf("list first partner page: %v", domainError)
	}
	if len(partnerFirst.Items) != 2 || partnerFirst.NextCursor == nil {
		t.Fatalf("expected paginated partner orders, got %+v", partnerFirst)
	}
	partnerSecond, domainError := application.ListPartnerOrders(ctx, venueID, "", *partnerFirst.NextCursor, 2)
	if domainError != nil || len(partnerSecond.Items) != 1 || partnerSecond.NextCursor != nil {
		t.Fatalf("unexpected second partner page: page=%+v err=%v", partnerSecond, domainError)
	}

	if _, domainError := application.ListPartnerOrders(ctx, venueID, "", *first.NextCursor, 2); domainError == nil || domainError.Code != "invalid_request" {
		t.Fatalf("expected customer cursor to be rejected by partner listing, got %v", domainError)
	}
	if _, domainError := application.ListCustomerOrders(ctx, "pagination-user", "not-a-cursor", 2); domainError == nil || domainError.Code != "invalid_request" {
		t.Fatalf("expected malformed cursor error, got %v", domainError)
	}
}

func TestConcurrentOrderCommandsCommitOnlyOneTransition(t *testing.T) {
	ctx := context.Background()
	application := service.New(memory.New())
	venueID := uuid.MustParse(memory.DemoVenueID)
	menu, domainError := application.GetMenu(ctx, venueID)
	if domainError != nil {
		t.Fatalf("get menu: %v", domainError)
	}
	created, domainError := application.CreateOrder(ctx, "user-1", "create-key", platformapi.CreateOrderRequest{
		VenueId:     venueID,
		MenuVersion: menu.Version,
		Items:       []platformapi.OrderItemInput{{ProductId: menu.Categories[0].Items[0].Id, Quantity: 1}},
		DeliveryAddress: platformapi.DeliveryAddress{
			City:        "Новосибирск",
			AddressLine: "Красный проспект, 10",
		},
	})
	if domainError != nil {
		t.Fatalf("create order: %v", domainError)
	}
	acceptRequest := platformapi.AcceptOrderRequest{VenueOrderId: "venue-order-1"}
	accepted, domainError := application.AcceptPartnerOrder(ctx, venueID, created.Id, "accept-key", acceptRequest)
	if domainError != nil {
		t.Fatalf("accept order: %v", domainError)
	}
	if accepted.Status != platformapi.OrderStatusAccepted {
		t.Fatalf("expected accepted order, got %s", accepted.Status)
	}

	var wait sync.WaitGroup
	results := make(chan *domain.Error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, apiError := application.CancelCustomerOrder(ctx, "user-1", created.Id, "cancel-key", nil)
		results <- apiError
	}()
	go func() {
		defer wait.Done()
		_, apiError := application.UpdatePartnerOrderStatus(ctx, venueID, created.Id, "prepare-key", platformapi.UpdateOrderStatusRequest{Status: platformapi.UpdateOrderStatusRequestStatusPreparing})
		results <- apiError
	}()
	wait.Wait()
	close(results)

	successes := 0
	for apiError := range results {
		if apiError == nil {
			successes++
			continue
		}
		if apiError.Code != "invalid_order_transition" {
			t.Fatalf("expected transition conflict, got %v", apiError)
		}
	}
	if successes != 1 {
		t.Fatalf("expected one successful concurrent command, got %d", successes)
	}

	order, domainError := application.GetCustomerOrder(ctx, "user-1", created.Id)
	if domainError != nil {
		t.Fatalf("get final order: %v", domainError)
	}
	if order.Status != platformapi.OrderStatusCancelled && order.Status != platformapi.OrderStatusPreparing {
		t.Fatalf("unexpected final status: %s", order.Status)
	}
	if order.StatusHistory == nil || len(*order.StatusHistory) != 3 {
		t.Fatalf("expected exactly three status history entries, got %+v", order.StatusHistory)
	}

	events, domainError := application.ListPartnerOrderEvents(ctx, venueID, "", 20)
	if domainError != nil {
		t.Fatalf("list order events: %v", domainError)
	}
	if len(events.Items) != 3 {
		t.Fatalf("expected create, accept and one final event, got %d", len(events.Items))
	}
	if events.Items[2].Order.Status != order.Status {
		t.Fatalf("final event status %s differs from stored status %s", events.Items[2].Order.Status, order.Status)
	}

	replayed, domainError := application.AcceptPartnerOrder(ctx, venueID, created.Id, "accept-key", acceptRequest)
	if domainError != nil {
		t.Fatalf("replay accepted command: %v", domainError)
	}
	if replayed.Status != platformapi.OrderStatusAccepted {
		t.Fatalf("expected stored accepted response, got %s", replayed.Status)
	}
}

func TestMenuSyncRejectsDuplicateExternalIDs(t *testing.T) {
	ctx := context.Background()
	venueID := uuid.MustParse(memory.DemoVenueID)
	application := service.New(memory.New())
	item := func(externalID string) platformapi.MenuItemInput {
		return platformapi.MenuItemInput{ExternalItemId: externalID, Name: externalID, Price: platformapi.Money{Amount: 10000, Currency: platformapi.MoneyCurrencyRUB}, Available: true}
	}
	tests := []struct {
		name       string
		categories []platformapi.MenuCategoryInput
	}{
		{
			name: "category ID",
			categories: []platformapi.MenuCategoryInput{
				{ExternalCategoryId: "same", Name: "First", Items: []platformapi.MenuItemInput{item("first")}},
				{ExternalCategoryId: "same", Name: "Second", Items: []platformapi.MenuItemInput{item("second")}},
			},
		},
		{
			name: "item ID across categories",
			categories: []platformapi.MenuCategoryInput{
				{ExternalCategoryId: "first", Name: "First", Items: []platformapi.MenuItemInput{item("same")}},
				{ExternalCategoryId: "second", Name: "Second", Items: []platformapi.MenuItemInput{item("same")}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, domainError := application.SyncPartnerMenu(ctx, venueID, platformapi.MenuSyncRequest{MenuVersion: 2, Categories: test.categories})
			if domainError == nil || domainError.Code != "invalid_request" {
				t.Fatalf("expected duplicate ID to be invalid_request, got %v", domainError)
			}
		})
	}
}

func TestCreateOrderRejectsAmountAboveContractMaximum(t *testing.T) {
	ctx := context.Background()
	venueID := uuid.MustParse(memory.DemoVenueID)
	application := service.New(memory.New())
	synced, domainError := application.SyncPartnerMenu(ctx, venueID, platformapi.MenuSyncRequest{
		MenuVersion: 2,
		Categories: []platformapi.MenuCategoryInput{{
			ExternalCategoryId: "expensive",
			Name:               "Expensive",
			Items: []platformapi.MenuItemInput{{
				ExternalItemId: "expensive-item",
				Name:           "Expensive item",
				Price:          platformapi.Money{Amount: 1_000_000_000, Currency: platformapi.MoneyCurrencyRUB},
				Available:      true,
			}},
		}},
	})
	if domainError != nil || len(synced.Items) != 1 {
		t.Fatalf("sync expensive menu: response=%+v err=%v", synced, domainError)
	}
	_, domainError = application.CreateOrder(ctx, "amount-user", "amount-key", platformapi.CreateOrderRequest{
		VenueId:     venueID,
		MenuVersion: 2,
		Items:       []platformapi.OrderItemInput{{ProductId: synced.Items[0].ProductId, Quantity: 2}},
		DeliveryAddress: platformapi.DeliveryAddress{
			City:        "Новосибирск",
			AddressLine: "Maximum, 1",
		},
	})
	if domainError == nil || domainError.Code != "invalid_request" {
		t.Fatalf("expected oversized amount to be rejected, got %v", domainError)
	}
}
