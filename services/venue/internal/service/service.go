// Package service contains application use cases of the demo venue.
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	venueapi "mealroute/venue/internal/api/venue"
	"mealroute/venue/internal/domain"
	"mealroute/venue/internal/repository"

	"github.com/google/uuid"
)

// PlatformGateway is an outbound port. Its HTTP implementation belongs to the
// integration layer and can be replaced in tests or by another delivery mode.
type PlatformGateway interface {
	SyncMenu(ctx context.Context, menu venueapi.VenueMenu) (MenuSyncResult, error)
	PullOrderEvents(ctx context.Context, cursor string) (PartnerOrderEventPage, error)
	AcceptOrder(ctx context.Context, order venueapi.VenueOrder) error
	RejectOrder(ctx context.Context, order venueapi.VenueOrder, reason *string, idempotencyKey string) error
	PushStatus(ctx context.Context, order venueapi.VenueOrder, idempotencyKey string) error
}

type PlatformError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *PlatformError) Error() string {
	return fmt.Sprintf("platform returned HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
}

type MenuSyncResult struct {
	ProductMappings []repository.ProductMapping
}

type PartnerOrderEvent struct {
	ID         string
	OccurredAt time.Time
	Order      PartnerOrder
}

type PartnerOrder struct {
	PlatformOrderID uuid.UUID
	Status          venueapi.VenueOrderStatus
	Items           []PartnerOrderItem
	Total           venueapi.Money
	RejectionReason *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type PartnerOrderItem struct {
	ProductID string
	Name      string
	Quantity  int32
	UnitPrice venueapi.Money
}

type PartnerOrderEventPage struct {
	Items      []PartnerOrderEvent
	NextCursor *string
}

type Service struct {
	repository repository.Repository
	platform   PlatformGateway
	syncMu     sync.Mutex
	pending    map[string]PartnerOrderEvent
}

func New(repository repository.Repository, platform PlatformGateway) *Service {
	return &Service{repository: repository, platform: platform, pending: make(map[string]PartnerOrderEvent)}
}

func (s *Service) Menu(ctx context.Context) venueapi.VenueMenu {
	return s.repository.Menu(ctx)
}

func (s *Service) ListOrders(ctx context.Context, status, cursor string, limit int32) (venueapi.VenueOrderPage, *domain.Error) {
	position, apiError := decodeOrderCursor(cursor, status)
	if apiError != nil {
		return venueapi.VenueOrderPage{}, apiError
	}
	pageLimit := domain.NormalizeLimit(limit)
	items := s.repository.ListOrders(ctx, status, position, pageLimit+1)
	hasMore := int32(len(items)) > pageLimit
	if hasMore {
		items = items[:pageLimit]
	}
	page := venueapi.VenueOrderPage{Items: items}
	if hasMore {
		last := items[len(items)-1]
		nextCursor := encodeOrderCursor(status, last)
		page.NextCursor = &nextCursor
	}
	return page, nil
}

func (s *Service) GetOrder(ctx context.Context, venueOrderID string) (venueapi.VenueOrder, *domain.Error) {
	order, ok := s.repository.FindOrder(ctx, venueOrderID)
	if !ok {
		return venueapi.VenueOrder{}, domain.NewError("order_not_found", "order was not found")
	}
	return order, nil
}

func (s *Service) UpdateOrderStatus(ctx context.Context, venueOrderID string, status venueapi.VenueCommandStatus, reason *string) (venueapi.VenueOrder, *domain.Error) {
	if status != venueapi.VenueCommandStatusRejected && reason != nil {
		return venueapi.VenueOrder{}, domain.NewError("invalid_request", "reason is only allowed when status is rejected")
	}
	if status == venueapi.VenueCommandStatusRejected && reason != nil {
		trimmed := strings.TrimSpace(*reason)
		if trimmed == "" {
			return venueapi.VenueOrder{}, domain.NewError("invalid_request", "rejection reason must not be blank")
		}
		reason = &trimmed
	}
	order, ok := s.repository.FindOrder(ctx, venueOrderID)
	if !ok {
		return venueapi.VenueOrder{}, domain.NewError("order_not_found", "order was not found")
	}
	if !domain.CanChangeOrderStatus(string(order.Status), string(status)) {
		return venueapi.VenueOrder{}, domain.NewError("invalid_order_transition", "order status transition is not allowed")
	}
	order.Status = venueapi.VenueOrderStatus(status)
	order.RejectionReason = nil
	if status == venueapi.VenueCommandStatusRejected {
		order.RejectionReason = reason
	}

	if s.platform != nil {
		key := fmt.Sprintf("venue-status-%s-%s", venueOrderID, status)
		var err error
		switch status {
		case venueapi.VenueCommandStatusAccepted:
			err = s.platform.AcceptOrder(ctx, order)
		case venueapi.VenueCommandStatusRejected:
			err = s.platform.RejectOrder(ctx, order, reason, key)
		default:
			err = s.platform.PushStatus(ctx, order, key)
		}
		if err != nil {
			return venueapi.VenueOrder{}, domain.NewError("platform_unavailable", "platform did not confirm the status change; local status was not changed")
		}
	}
	s.repository.SaveOrder(ctx, order)
	return order, nil
}

// Run continuously synchronizes the local demo venue with the platform.
func (s *Service) Run(ctx context.Context, syncInterval time.Duration) {
	if s.platform == nil {
		log.Print("platform gateway is not configured; venue sync worker is disabled")
		return
	}
	if err := s.SyncWithPlatform(ctx); err != nil {
		log.Printf("initial venue sync failed: %v", err)
	}
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.SyncWithPlatform(ctx); err != nil {
				log.Printf("venue sync failed: %v", err)
			}
		}
	}
}

func (s *Service) SyncWithPlatform(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	if s.platform == nil {
		return nil
	}
	var failures []error
	menuSync, err := s.platform.SyncMenu(ctx, s.repository.Menu(ctx))
	if err != nil {
		failures = append(failures, fmt.Errorf("sync menu: %w", err))
	} else {
		s.repository.SaveProductMappings(ctx, menuSync.ProductMappings)
	}

	for _, event := range s.pendingEvents() {
		if err := s.processEvent(ctx, event); err != nil {
			failures = append(failures, fmt.Errorf("retry event %s: %w", event.ID, err))
			continue
		}
		delete(s.pending, event.ID)
		s.repository.MarkEventProcessed(ctx, event.ID)
	}

	page, err := s.platform.PullOrderEvents(ctx, s.repository.PartnerCursor(ctx))
	if err != nil {
		failures = append(failures, fmt.Errorf("pull order events: %w", err))
		return errors.Join(failures...)
	}
	for _, event := range page.Items {
		if s.repository.IsEventProcessed(ctx, event.ID) {
			continue
		}
		if err := s.processEvent(ctx, event); err != nil {
			s.pending[event.ID] = event
			failures = append(failures, fmt.Errorf("process event %s: %w", event.ID, err))
			continue
		}
		delete(s.pending, event.ID)
		s.repository.MarkEventProcessed(ctx, event.ID)
	}
	if page.NextCursor != nil {
		s.repository.SavePartnerCursor(ctx, *page.NextCursor)
	}
	return errors.Join(failures...)
}

func (s *Service) pendingEvents() []PartnerOrderEvent {
	events := make([]PartnerOrderEvent, 0, len(s.pending))
	for _, event := range s.pending {
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].ID < events[j].ID
		}
		return events[i].OccurredAt.Before(events[j].OccurredAt)
	})
	return events
}

func (s *Service) processEvent(ctx context.Context, event PartnerOrderEvent) error {
	order, err := s.toVenueOrder(ctx, event.Order)
	if err != nil {
		return fmt.Errorf("map platform order %s to venue items: %w", event.Order.PlatformOrderID, err)
	}
	if current, found := s.repository.FindOrder(ctx, order.VenueOrderId); found && !order.UpdatedAt.After(current.UpdatedAt) {
		return nil
	}
	s.repository.SaveOrder(ctx, order)
	return nil
}

func (s *Service) toVenueOrder(ctx context.Context, order PartnerOrder) (venueapi.VenueOrder, error) {
	items := make([]venueapi.VenueOrderItem, 0, len(order.Items))
	for _, item := range order.Items {
		externalItemID, found := s.repository.ExternalItemID(ctx, item.ProductID)
		if !found {
			return venueapi.VenueOrder{}, fmt.Errorf("external item ID for product %q is not mapped", item.ProductID)
		}
		items = append(items, venueapi.VenueOrderItem{ExternalItemId: externalItemID, Name: item.Name, Quantity: item.Quantity, UnitPrice: item.UnitPrice})
	}
	return venueapi.VenueOrder{
		VenueOrderId:    venueOrderID(order.PlatformOrderID),
		PlatformOrderId: order.PlatformOrderID,
		Status:          order.Status,
		Items:           items,
		Total:           order.Total,
		RejectionReason: rejectionReason(order),
		CreatedAt:       order.CreatedAt,
		UpdatedAt:       order.UpdatedAt,
	}, nil
}

func venueOrderID(platformOrderID uuid.UUID) string {
	derived := uuid.NewSHA1(uuid.NameSpaceURL, []byte("mealroute:venue-order:"+platformOrderID.String()))
	return "venue-" + derived.String()
}

func rejectionReason(order PartnerOrder) *string {
	if order.Status != venueapi.VenueOrderStatusRejected {
		return nil
	}
	return order.RejectionReason
}
