// Package service contains platform application use cases.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/domain"
	"mealroute/platform/internal/repository"

	"github.com/google/uuid"
)

type Service struct {
	repository repository.Repository
	now        func() time.Time
	newID      func() uuid.UUID
}

const (
	venueCursorKind               = "venues-v1"
	customerOrderCursorKind       = "customer-orders-v1"
	partnerOrderCursorKind        = "partner-orders-v1"
	maxMoneyAmount          int64 = 1_000_000_000
)

type pageCursor struct {
	Version   int        `json:"v"`
	Kind      string     `json:"kind"`
	Scope     string     `json:"scope"`
	Name      string     `json:"name,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	ID        uuid.UUID  `json:"id"`
}

func New(repository repository.Repository) *Service {
	return &Service{repository: repository, now: func() time.Time { return time.Now().UTC() }, newID: uuid.New}
}

// AuthenticateVenue resolves a partner API key to its venue. The returned ID
// is the tenant boundary for every partner operation.
func (s *Service) AuthenticateVenue(ctx context.Context, apiKey string) (uuid.UUID, *domain.Error) {
	if apiKey == "" {
		return uuid.Nil, domain.NewError("invalid_venue_api_key", "venue API key is missing or invalid")
	}
	venueID, found, err := s.repository.FindVenueIDByAPIKey(ctx, apiKey)
	if err != nil {
		return uuid.Nil, storageError(err)
	}
	if !found {
		return uuid.Nil, domain.NewError("invalid_venue_api_key", "venue API key is missing or invalid")
	}
	return venueID, nil
}

func (s *Service) ListVenues(ctx context.Context, city, cursor string, limit int32) (platformapi.VenuePage, *domain.Error) {
	scope := pageCursorScope("venues", strings.ToLower(city))
	position, apiError := decodeVenueCursor(cursor, scope)
	if apiError != nil {
		return platformapi.VenuePage{}, apiError
	}
	pageLimit := domain.NormalizeLimit(limit)
	venues, err := s.repository.ListVenues(ctx, city, position, pageLimit+1)
	if err != nil {
		return platformapi.VenuePage{}, storageError(err)
	}
	hasMore := int32(len(venues)) > pageLimit
	if hasMore {
		venues = venues[:pageLimit]
	}
	items := make([]platformapi.VenueSummary, 0, len(venues))
	for _, venue := range venues {
		items = append(items, platformapi.VenueSummary{Id: venue.Id, Name: venue.Name, Kind: venue.Kind, City: venue.City, Status: venue.Status, IsOpen: venue.IsOpen})
	}
	page := platformapi.VenuePage{Items: items}
	if hasMore {
		last := venues[len(venues)-1]
		nextCursor := encodePageCursor(pageCursor{Version: 1, Kind: venueCursorKind, Scope: scope, Name: last.Name, ID: last.Id})
		page.NextCursor = &nextCursor
	}
	return page, nil
}

func (s *Service) GetVenue(ctx context.Context, venueID uuid.UUID) (platformapi.Venue, *domain.Error) {
	venue, ok, err := s.repository.FindVenue(ctx, venueID)
	if err != nil {
		return platformapi.Venue{}, storageError(err)
	}
	if !ok {
		return platformapi.Venue{}, domain.NewError("venue_not_found", "venue was not found")
	}
	return venue, nil
}

func (s *Service) GetMenu(ctx context.Context, venueID uuid.UUID) (platformapi.Menu, *domain.Error) {
	menu, ok, err := s.repository.FindMenu(ctx, venueID)
	if err != nil {
		return platformapi.Menu{}, storageError(err)
	}
	if !ok {
		return platformapi.Menu{}, domain.NewError("venue_not_found", "venue was not found")
	}
	menu.Categories = availableCategories(menu.Categories)
	return menu, nil
}

func (s *Service) ListCustomerOrders(ctx context.Context, userID, cursor string, limit int32) (platformapi.OrderPage, *domain.Error) {
	scope := pageCursorScope("customer-orders", userID)
	position, apiError := decodeOrderCursor(cursor, customerOrderCursorKind, scope)
	if apiError != nil {
		return platformapi.OrderPage{}, apiError
	}
	pageLimit := domain.NormalizeLimit(limit)
	orders, err := s.repository.ListOrdersForUser(ctx, userID, position, pageLimit+1)
	if err != nil {
		return platformapi.OrderPage{}, storageError(err)
	}
	return buildOrderPage(orders, pageLimit, customerOrderCursorKind, scope), nil
}

func (s *Service) CreateOrder(ctx context.Context, userID, key string, request platformapi.CreateOrderRequest) (platformapi.Order, *domain.Error) {
	if key == "" {
		return platformapi.Order{}, domain.NewError("invalid_request", "Idempotency-Key must not be empty")
	}
	requestHash := commandHash(request)
	if existing, replayed, apiError := s.replayCommand(ctx, "user:"+userID, "create_order", key, requestHash); replayed || apiError != nil {
		return existing, apiError
	}
	if len(request.Items) == 0 {
		return platformapi.Order{}, domain.NewError("invalid_request", "order must contain at least one item")
	}

	venue, ok, err := s.repository.FindVenue(ctx, request.VenueId)
	if err != nil {
		return platformapi.Order{}, storageError(err)
	}
	if !ok {
		return platformapi.Order{}, domain.NewError("venue_not_found", "venue was not found")
	}
	if venue.Status != platformapi.Active || !venue.IsOpen {
		return platformapi.Order{}, domain.NewError("venue_closed", "venue is not accepting orders")
	}
	menu, ok, err := s.repository.FindMenu(ctx, request.VenueId)
	if err != nil {
		return platformapi.Order{}, storageError(err)
	}
	if !ok {
		return platformapi.Order{}, domain.NewError("venue_not_found", "venue was not found")
	}
	if request.MenuVersion != menu.Version {
		return platformapi.Order{}, domain.NewError("menu_version_mismatch", "menu has changed; refresh the menu")
	}

	items := make([]platformapi.OrderItem, 0, len(request.Items))
	var subtotal int64
	for _, requested := range request.Items {
		if requested.Quantity <= 0 {
			return platformapi.Order{}, domain.NewError("invalid_request", "item quantity must be positive")
		}
		menuItem, found := findMenuItem(menu.Categories, requested.ProductId)
		if !found {
			return platformapi.Order{}, domain.NewError("item_not_found", "one of the requested items is not in the menu")
		}
		if !menuItem.Available {
			return platformapi.Order{}, domain.NewError("item_unavailable", fmt.Sprintf("item %q is unavailable", menuItem.Name))
		}
		quantity := int64(requested.Quantity)
		if menuItem.Price.Amount < 0 || menuItem.Price.Amount > maxMoneyAmount/quantity {
			return platformapi.Order{}, domain.NewError("invalid_request", "order amount exceeds the supported maximum")
		}
		lineTotal := menuItem.Price.Amount * quantity
		if subtotal > maxMoneyAmount-lineTotal {
			return platformapi.Order{}, domain.NewError("invalid_request", "order amount exceeds the supported maximum")
		}
		subtotal += lineTotal
		items = append(items, platformapi.OrderItem{ProductId: menuItem.Id, Name: menuItem.Name, Quantity: requested.Quantity, UnitPrice: menuItem.Price, TotalPrice: platformapi.Money{Amount: lineTotal, Currency: platformapi.MoneyCurrencyRUB}})
	}

	now := s.now()
	deliveryFee := platformapi.Money{Amount: 19900, Currency: platformapi.MoneyCurrencyRUB}
	if subtotal > maxMoneyAmount-deliveryFee.Amount {
		return platformapi.Order{}, domain.NewError("invalid_request", "order amount exceeds the supported maximum")
	}
	statusHistory := []platformapi.OrderStatusChange{{Status: platformapi.OrderStatusPendingConfirmation, Source: platformapi.OrderStatusChangeSourcePlatform, ChangedAt: now}}
	order := platformapi.Order{Id: s.newID(), VenueId: venue.Id, VenueName: venue.Name, Status: platformapi.OrderStatusPendingConfirmation, Items: items, Subtotal: platformapi.Money{Amount: subtotal, Currency: platformapi.MoneyCurrencyRUB}, DeliveryFee: deliveryFee, Total: platformapi.Money{Amount: subtotal + deliveryFee.Amount, Currency: platformapi.MoneyCurrencyRUB}, DeliveryAddress: request.DeliveryAddress, CustomerComment: request.CustomerComment, StatusHistory: &statusHistory, CreatedAt: now, UpdatedAt: now}
	command := repository.OrderCommand{
		Subject:        "user:" + userID,
		Operation:      "create_order",
		IdempotencyKey: key,
		RequestHash:    requestHash,
		MenuVersion:    request.MenuVersion,
		Order:          order,
		Event:          platformapi.OrderEvent{EventId: s.newID(), Type: platformapi.OrderEventTypeCreated, OccurredAt: now, Order: order},
	}
	result, err := s.repository.CreateOrderCommand(ctx, userID, command)
	if err != nil {
		return platformapi.Order{}, storageError(err)
	}
	return commandResult(result)
}

func (s *Service) GetCustomerOrder(ctx context.Context, userID string, orderID uuid.UUID) (platformapi.Order, *domain.Error) {
	order, ok, err := s.repository.FindOrderForUser(ctx, userID, orderID)
	if err != nil {
		return platformapi.Order{}, storageError(err)
	}
	if !ok {
		return platformapi.Order{}, domain.NewError("order_not_found", "order was not found")
	}
	return order, nil
}

func (s *Service) CancelCustomerOrder(ctx context.Context, userID string, orderID uuid.UUID, key string, reason *string) (platformapi.Order, *domain.Error) {
	if key == "" {
		return platformapi.Order{}, domain.NewError("invalid_request", "Idempotency-Key must not be empty")
	}
	requestHash := commandHash(platformapi.CancelOrderRequest{Reason: reason})
	operation := "cancel_order:" + orderID.String()
	if existing, replayed, apiError := s.replayCommand(ctx, "user:"+userID, operation, key, requestHash); replayed || apiError != nil {
		return existing, apiError
	}
	order, ok, err := s.repository.FindOrderForUser(ctx, userID, orderID)
	if err != nil {
		return platformapi.Order{}, storageError(err)
	}
	if !ok {
		return platformapi.Order{}, domain.NewError("order_not_found", "order was not found")
	}
	if !domain.CanCancelOrder(string(order.Status)) {
		return platformapi.Order{}, domain.NewError("invalid_order_transition", "order cannot be cancelled in its current status")
	}
	expectedStatus := order.Status
	now := s.now()
	setOrderStatus(&order, platformapi.OrderStatusCancelled, platformapi.OrderStatusChangeSourceCustomer, reason, now)
	return s.applyOrderCommand(ctx, repository.OrderCommand{
		Subject:        "user:" + userID,
		Operation:      operation,
		IdempotencyKey: key,
		RequestHash:    requestHash,
		ExpectedStatus: expectedStatus,
		Order:          order,
		Event:          platformapi.OrderEvent{EventId: s.newID(), Type: platformapi.OrderEventTypeCancelled, OccurredAt: now, Order: order},
	})
}

func (s *Service) ListPartnerOrders(ctx context.Context, venueID uuid.UUID, status, cursor string, limit int32) (platformapi.OrderPage, *domain.Error) {
	scopeStatus := status
	if scopeStatus == "" {
		scopeStatus = string(platformapi.OrderStatusPendingConfirmation)
	}
	scope := pageCursorScope("partner-orders", venueID.String(), scopeStatus)
	position, apiError := decodeOrderCursor(cursor, partnerOrderCursorKind, scope)
	if apiError != nil {
		return platformapi.OrderPage{}, apiError
	}
	pageLimit := domain.NormalizeLimit(limit)
	orders, err := s.repository.ListOrdersForPartner(ctx, venueID, status, position, pageLimit+1)
	if err != nil {
		return platformapi.OrderPage{}, storageError(err)
	}
	return buildOrderPage(orders, pageLimit, partnerOrderCursorKind, scope), nil
}

func (s *Service) ListPartnerOrderEvents(ctx context.Context, venueID uuid.UUID, cursor string, limit int32) (platformapi.OrderEventPage, *domain.Error) {
	var afterSequence uint64
	if cursor != "" {
		parsed, err := strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			return platformapi.OrderEventPage{}, domain.NewError("invalid_request", "cursor must be a valid event cursor")
		}
		afterSequence = parsed
	}
	events, lastSequence, hasEvents, err := s.repository.ListOrderEvents(ctx, venueID, afterSequence, domain.NormalizeLimit(limit))
	if err != nil {
		return platformapi.OrderEventPage{}, storageError(err)
	}
	page := platformapi.OrderEventPage{Items: events}
	if hasEvents {
		nextCursor := strconv.FormatUint(lastSequence, 10)
		page.NextCursor = &nextCursor
	}
	return page, nil
}

func (s *Service) AcceptPartnerOrder(ctx context.Context, venueID, orderID uuid.UUID, key string, request platformapi.AcceptOrderRequest) (platformapi.Order, *domain.Error) {
	if key == "" {
		return platformapi.Order{}, domain.NewError("invalid_request", "Idempotency-Key must not be empty")
	}
	requestHash := commandHash(request)
	operation := "accept_order:" + orderID.String()
	subject := "venue:" + venueID.String()
	if existing, replayed, apiError := s.replayCommand(ctx, subject, operation, key, requestHash); replayed || apiError != nil {
		return existing, apiError
	}
	if request.VenueOrderId == "" {
		return platformapi.Order{}, domain.NewError("invalid_request", "venueOrderId must not be empty")
	}
	order, ok, err := s.repository.FindOrderForVenue(ctx, venueID, orderID)
	if err != nil {
		return platformapi.Order{}, storageError(err)
	}
	if !ok {
		return platformapi.Order{}, domain.NewError("order_not_found", "order was not found")
	}
	if order.Status == platformapi.OrderStatusAccepted {
		existingVenueOrderID, ok, err := s.repository.VenueOrderID(ctx, order.Id)
		if err != nil {
			return platformapi.Order{}, storageError(err)
		}
		if ok && existingVenueOrderID == request.VenueOrderId {
			return order, nil
		}
		return platformapi.Order{}, domain.NewError("idempotency_key_reused", "order was already accepted with another venue order id")
	}
	if order.Status != platformapi.OrderStatusPendingConfirmation {
		return platformapi.Order{}, domain.NewError("invalid_order_transition", "only pending orders can be accepted")
	}
	expectedStatus := order.Status
	now := s.now()
	setOrderStatus(&order, platformapi.OrderStatusAccepted, platformapi.OrderStatusChangeSourceVenue, nil, now)
	if request.EstimatedPreparationMinutes != nil {
		readyAt := now.Add(time.Duration(*request.EstimatedPreparationMinutes) * time.Minute)
		order.EstimatedReadyAt = &readyAt
	}
	venueOrderID := request.VenueOrderId
	return s.applyOrderCommand(ctx, repository.OrderCommand{
		Subject:        subject,
		Operation:      operation,
		IdempotencyKey: key,
		RequestHash:    requestHash,
		ExpectedStatus: expectedStatus,
		Order:          order,
		Event:          platformapi.OrderEvent{EventId: s.newID(), Type: platformapi.OrderEventTypeAccepted, OccurredAt: now, Order: order},
		VenueOrderID:   &venueOrderID,
	})
}

func (s *Service) RejectPartnerOrder(ctx context.Context, venueID, orderID uuid.UUID, key string, request platformapi.RejectOrderRequest) (platformapi.Order, *domain.Error) {
	if key == "" {
		return platformapi.Order{}, domain.NewError("invalid_request", "Idempotency-Key must not be empty")
	}
	requestHash := commandHash(request)
	operation := "reject_order:" + orderID.String()
	subject := "venue:" + venueID.String()
	if existing, replayed, apiError := s.replayCommand(ctx, subject, operation, key, requestHash); replayed || apiError != nil {
		return existing, apiError
	}
	order, ok, apiError := s.findPartnerOrder(ctx, venueID, orderID)
	if apiError != nil {
		return platformapi.Order{}, apiError
	}
	if !ok {
		return platformapi.Order{}, domain.NewError("order_not_found", "order was not found")
	}
	if order.Status != platformapi.OrderStatusPendingConfirmation {
		return platformapi.Order{}, domain.NewError("invalid_order_transition", "only pending orders can be rejected")
	}
	message := string(request.Reason)
	if request.Comment != nil && *request.Comment != "" {
		message = *request.Comment
	}
	expectedStatus := order.Status
	now := s.now()
	setOrderStatus(&order, platformapi.OrderStatusRejected, platformapi.OrderStatusChangeSourceVenue, &message, now)
	return s.applyOrderCommand(ctx, repository.OrderCommand{
		Subject:        subject,
		Operation:      operation,
		IdempotencyKey: key,
		RequestHash:    requestHash,
		ExpectedStatus: expectedStatus,
		Order:          order,
		Event:          platformapi.OrderEvent{EventId: s.newID(), Type: platformapi.OrderEventTypeRejected, OccurredAt: now, Order: order},
	})
}

func (s *Service) UpdatePartnerOrderStatus(ctx context.Context, venueID, orderID uuid.UUID, key string, request platformapi.UpdateOrderStatusRequest) (platformapi.Order, *domain.Error) {
	if key == "" {
		return platformapi.Order{}, domain.NewError("invalid_request", "Idempotency-Key must not be empty")
	}
	requestHash := commandHash(request)
	operation := "update_order_status:" + orderID.String()
	subject := "venue:" + venueID.String()
	if existing, replayed, apiError := s.replayCommand(ctx, subject, operation, key, requestHash); replayed || apiError != nil {
		return existing, apiError
	}
	order, ok, apiError := s.findPartnerOrder(ctx, venueID, orderID)
	if apiError != nil {
		return platformapi.Order{}, apiError
	}
	if !ok {
		return platformapi.Order{}, domain.NewError("order_not_found", "order was not found")
	}
	next := platformapi.OrderStatus(request.Status)
	if !domain.CanVenueAdvanceOrder(string(order.Status), string(next)) {
		return platformapi.Order{}, domain.NewError("invalid_order_transition", "order status transition is not allowed")
	}
	expectedStatus := order.Status
	now := s.now()
	setOrderStatus(&order, next, platformapi.OrderStatusChangeSourceVenue, nil, now)
	return s.applyOrderCommand(ctx, repository.OrderCommand{
		Subject:        subject,
		Operation:      operation,
		IdempotencyKey: key,
		RequestHash:    requestHash,
		ExpectedStatus: expectedStatus,
		Order:          order,
		Event:          platformapi.OrderEvent{EventId: s.newID(), Type: platformapi.OrderEventTypeStatusChanged, OccurredAt: now, Order: order},
	})
}

func (s *Service) SyncPartnerMenu(ctx context.Context, venueID uuid.UUID, request platformapi.MenuSyncRequest) (platformapi.MenuSyncResponse, *domain.Error) {
	if apiError := validateMenuSyncRequest(request); apiError != nil {
		return platformapi.MenuSyncResponse{}, apiError
	}
	venue, ok, err := s.repository.FindVenue(ctx, venueID)
	if err != nil {
		return platformapi.MenuSyncResponse{}, storageError(err)
	}
	if !ok {
		return platformapi.MenuSyncResponse{}, domain.NewError("venue_not_found", "venue was not found")
	}
	current, ok, err := s.repository.FindMenu(ctx, venueID)
	if err != nil {
		return platformapi.MenuSyncResponse{}, storageError(err)
	}
	if !ok {
		return platformapi.MenuSyncResponse{}, domain.NewError("venue_not_found", "venue was not found")
	}
	if request.MenuVersion <= current.Version {
		mappings, err := s.repository.ProductMappings(ctx, venueID)
		if err != nil {
			return platformapi.MenuSyncResponse{}, storageError(err)
		}
		return menuSyncResponse(current, venueID, mappings), nil
	}

	productIDs := make(map[string]uuid.UUID)
	categories := make([]platformapi.MenuCategory, 0, len(request.Categories))
	for _, inputCategory := range request.Categories {
		categoryID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("category:"+venueID.String()+":"+inputCategory.ExternalCategoryId))
		items := make([]platformapi.MenuItem, 0, len(inputCategory.Items))
		for _, inputItem := range inputCategory.Items {
			productID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("product:"+venueID.String()+":"+inputItem.ExternalItemId))
			productIDs[inputItem.ExternalItemId] = productID
			items = append(items, platformapi.MenuItem{Id: productID, Name: inputItem.Name, Description: inputItem.Description, Price: inputItem.Price, Available: inputItem.Available, SortOrder: inputItem.SortOrder, Tags: inputItem.Tags})
		}
		categories = append(categories, platformapi.MenuCategory{Id: categoryID, Name: inputCategory.Name, SortOrder: inputCategory.SortOrder, Items: items})
	}
	now := s.now()
	menu := platformapi.Menu{VenueId: venueID, Version: request.MenuVersion, Currency: platformapi.MenuCurrencyRUB, Categories: categories, UpdatedAt: now}
	venue.MenuVersion = menu.Version
	venue.UpdatedAt = now
	applied, err := s.repository.SaveMenuSnapshot(ctx, venue, menu, productIDs)
	if err != nil {
		return platformapi.MenuSyncResponse{}, storageError(err)
	}
	if !applied {
		current, ok, err := s.repository.FindMenu(ctx, venueID)
		if err != nil {
			return platformapi.MenuSyncResponse{}, storageError(err)
		}
		if !ok {
			return platformapi.MenuSyncResponse{}, domain.NewError("venue_not_found", "venue was not found")
		}
		mappings, err := s.repository.ProductMappings(ctx, venueID)
		if err != nil {
			return platformapi.MenuSyncResponse{}, storageError(err)
		}
		return menuSyncResponse(current, venueID, mappings), nil
	}
	return menuSyncResponse(menu, venueID, productIDs), nil
}

func (s *Service) findPartnerOrder(ctx context.Context, venueID, orderID uuid.UUID) (platformapi.Order, bool, *domain.Error) {
	order, found, err := s.repository.FindOrderForVenue(ctx, venueID, orderID)
	if err != nil {
		return platformapi.Order{}, false, storageError(err)
	}
	return order, found, nil
}

func (s *Service) applyOrderCommand(ctx context.Context, command repository.OrderCommand) (platformapi.Order, *domain.Error) {
	result, err := s.repository.ApplyOrderCommand(ctx, command)
	if err != nil {
		return platformapi.Order{}, storageError(err)
	}
	return commandResult(result)
}

func commandResult(result repository.OrderCommandResult) (platformapi.Order, *domain.Error) {
	if result.RequestHashMismatch {
		return platformapi.Order{}, domain.NewError("idempotency_key_reused", "Idempotency-Key was already used with another request body")
	}
	if result.MenuVersionConflict {
		return platformapi.Order{}, domain.NewError("menu_version_mismatch", "menu has changed; refresh the menu")
	}
	if result.StateConflict {
		return platformapi.Order{}, domain.NewError("invalid_order_transition", "order was changed concurrently; refresh its status")
	}
	return result.Order, nil
}

func (s *Service) replayCommand(ctx context.Context, subject, operation, key, requestHash string) (platformapi.Order, bool, *domain.Error) {
	order, storedHash, found, err := s.repository.FindCommand(ctx, subject, operation, key)
	if err != nil {
		return platformapi.Order{}, false, storageError(err)
	}
	if !found {
		return platformapi.Order{}, false, nil
	}
	if storedHash != requestHash {
		return platformapi.Order{}, false, domain.NewError("idempotency_key_reused", "Idempotency-Key was already used with another request body")
	}
	return order, true, nil
}

func commandHash(value any) string {
	payload, _ := json.Marshal(value)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func pageCursorScope(parts ...string) string {
	payload, _ := json.Marshal(parts)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func encodePageCursor(cursor pageCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeVenueCursor(raw, scope string) (*repository.VenueCursor, *domain.Error) {
	cursor, apiError := decodePageCursor(raw, venueCursorKind, scope)
	if apiError != nil || cursor == nil {
		return nil, apiError
	}
	if cursor.Name == "" || cursor.CreatedAt != nil {
		return nil, invalidCursorError()
	}
	return &repository.VenueCursor{Name: cursor.Name, ID: cursor.ID}, nil
}

func decodeOrderCursor(raw, kind, scope string) (*repository.OrderCursor, *domain.Error) {
	cursor, apiError := decodePageCursor(raw, kind, scope)
	if apiError != nil || cursor == nil {
		return nil, apiError
	}
	if cursor.CreatedAt == nil || cursor.CreatedAt.IsZero() || cursor.Name != "" {
		return nil, invalidCursorError()
	}
	return &repository.OrderCursor{CreatedAt: *cursor.CreatedAt, ID: cursor.ID}, nil
}

func decodePageCursor(raw, kind, scope string) (*pageCursor, *domain.Error) {
	if raw == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, invalidCursorError()
	}
	var cursor pageCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return nil, invalidCursorError()
	}
	if cursor.Version != 1 || cursor.Kind != kind || cursor.Scope != scope || cursor.ID == uuid.Nil {
		return nil, invalidCursorError()
	}
	return &cursor, nil
}

func buildOrderPage(orders []platformapi.Order, limit int32, kind, scope string) platformapi.OrderPage {
	hasMore := int32(len(orders)) > limit
	if hasMore {
		orders = orders[:limit]
	}
	page := platformapi.OrderPage{Items: orders}
	if hasMore {
		last := orders[len(orders)-1]
		createdAt := last.CreatedAt
		nextCursor := encodePageCursor(pageCursor{Version: 1, Kind: kind, Scope: scope, CreatedAt: &createdAt, ID: last.Id})
		page.NextCursor = &nextCursor
	}
	return page
}

func invalidCursorError() *domain.Error {
	return domain.NewError("invalid_request", "cursor is invalid for this request")
}

func storageError(err error) *domain.Error {
	return domain.NewError("internal_error", "storage operation failed")
}

func setOrderStatus(order *platformapi.Order, status platformapi.OrderStatus, source platformapi.OrderStatusChangeSource, reason *string, now time.Time) {
	order.Status = status
	order.UpdatedAt = now
	order.RejectionReason = nil
	if status == platformapi.OrderStatusRejected {
		order.RejectionReason = reason
	}
	changes := []platformapi.OrderStatusChange(nil)
	if order.StatusHistory != nil {
		changes = append(changes, (*order.StatusHistory)...)
	}
	changes = append(changes, platformapi.OrderStatusChange{Status: status, Source: source, Reason: reason, ChangedAt: now})
	order.StatusHistory = &changes
}

func validateMenuSyncRequest(request platformapi.MenuSyncRequest) *domain.Error {
	categoryIDs := make(map[string]struct{}, len(request.Categories))
	itemIDs := make(map[string]struct{})
	for _, category := range request.Categories {
		if _, exists := categoryIDs[category.ExternalCategoryId]; exists {
			return domain.NewError("invalid_request", fmt.Sprintf("duplicate externalCategoryId %q", category.ExternalCategoryId))
		}
		categoryIDs[category.ExternalCategoryId] = struct{}{}
		for _, item := range category.Items {
			if _, exists := itemIDs[item.ExternalItemId]; exists {
				return domain.NewError("invalid_request", fmt.Sprintf("duplicate externalItemId %q", item.ExternalItemId))
			}
			itemIDs[item.ExternalItemId] = struct{}{}
			if item.Price.Amount < 0 || item.Price.Amount > maxMoneyAmount {
				return domain.NewError("invalid_request", fmt.Sprintf("price amount for externalItemId %q exceeds the supported range", item.ExternalItemId))
			}
		}
	}
	return nil
}

func findMenuItem(categories []platformapi.MenuCategory, id uuid.UUID) (platformapi.MenuItem, bool) {
	for _, category := range categories {
		for _, item := range category.Items {
			if item.Id == id {
				return item, true
			}
		}
	}
	return platformapi.MenuItem{}, false
}

func availableCategories(categories []platformapi.MenuCategory) []platformapi.MenuCategory {
	result := make([]platformapi.MenuCategory, 0, len(categories))
	for _, category := range categories {
		items := make([]platformapi.MenuItem, 0, len(category.Items))
		for _, item := range category.Items {
			if item.Available {
				items = append(items, item)
			}
		}
		if len(items) > 0 {
			category.Items = items
			result = append(result, category)
		}
	}
	return result
}

func menuSyncResponse(menu platformapi.Menu, venueID uuid.UUID, products map[string]uuid.UUID) platformapi.MenuSyncResponse {
	externalIDs := make([]string, 0, len(products))
	for externalID := range products {
		externalIDs = append(externalIDs, externalID)
	}
	sort.Strings(externalIDs)
	mappings := make([]platformapi.MenuItemMapping, 0, len(externalIDs))
	for _, externalID := range externalIDs {
		mappings = append(mappings, platformapi.MenuItemMapping{ExternalItemId: externalID, ProductId: products[externalID]})
	}
	return platformapi.MenuSyncResponse{VenueId: venueID, MenuVersion: menu.Version, AcceptedAt: menu.UpdatedAt, Items: mappings}
}
