// Package memory is the in-memory implementation of platform persistence ports
// used by unit tests. It owns maps and locking, but not business decisions.
package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/repository"

	"github.com/google/uuid"
)

const DemoVenueID = "00000000-0000-0000-0000-000000000001"

type Repository struct {
	mu              sync.RWMutex
	venues          map[uuid.UUID]platformapi.Venue
	menus           map[uuid.UUID]platformapi.Menu
	venueAPIKeys    map[string]uuid.UUID
	orders          map[uuid.UUID]platformapi.Order
	orderUsers      map[uuid.UUID]string
	venueOrder      []uuid.UUID
	orderOrder      []uuid.UUID
	commands        map[string]commandRecord
	venueOrderIDs   map[uuid.UUID]string
	events          []eventRecord
	nextEventSeq    uint64
	externalProduct map[uuid.UUID]map[string]uuid.UUID
}

type eventRecord struct {
	sequence uint64
	event    platformapi.OrderEvent
}

type commandRecord struct {
	response    platformapi.Order
	requestHash string
}

func New() *Repository {
	venueID := uuid.MustParse(DemoVenueID)
	categoryID := uuid.MustParse("00000000-0000-0000-0000-000000000010")
	pepperoniID := uuid.MustParse("00000000-0000-0000-0000-000000000011")
	margheritaID := uuid.MustParse("00000000-0000-0000-0000-000000000012")
	sortZero := int32(0)
	now := time.Now().UTC()

	menu := platformapi.Menu{
		VenueId:  venueID,
		Version:  1,
		Currency: platformapi.MenuCurrencyRUB,
		Categories: []platformapi.MenuCategory{{
			Id:        categoryID,
			Name:      "Пицца",
			SortOrder: &sortZero,
			Items: []platformapi.MenuItem{
				{Id: pepperoniID, Name: "Пепперони", Description: stringPtr("Острая салями, моцарелла и томатный соус"), Price: platformapi.Money{Amount: 54900, Currency: platformapi.MoneyCurrencyRUB}, Available: true, SortOrder: &sortZero},
				{Id: margheritaID, Name: "Маргарита", Description: stringPtr("Моцарелла, томаты и базилик"), Price: platformapi.Money{Amount: 44900, Currency: platformapi.MoneyCurrencyRUB}, Available: true, SortOrder: &sortZero},
			},
		}},
		UpdatedAt: now,
	}

	return &Repository{
		venues: map[uuid.UUID]platformapi.Venue{
			venueID: {Id: venueID, Name: "Демо Пицца", Kind: platformapi.Restaurant, City: "Новосибирск", Address: "Красный проспект, 1", Status: platformapi.Active, IsOpen: true, MenuVersion: menu.Version, UpdatedAt: now},
		},
		menus:           map[uuid.UUID]platformapi.Menu{venueID: menu},
		venueAPIKeys:    make(map[string]uuid.UUID),
		orders:          make(map[uuid.UUID]platformapi.Order),
		orderUsers:      make(map[uuid.UUID]string),
		venueOrder:      []uuid.UUID{venueID},
		commands:        make(map[string]commandRecord),
		venueOrderIDs:   make(map[uuid.UUID]string),
		events:          []eventRecord{},
		externalProduct: make(map[uuid.UUID]map[string]uuid.UUID),
	}
}

func NewWithVenueAPIKey(apiKey string) *Repository {
	repository := New()
	repository.venueAPIKeys[apiKey] = uuid.MustParse(DemoVenueID)
	return repository
}

func (r *Repository) FindVenueIDByAPIKey(_ context.Context, apiKey string) (uuid.UUID, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	venueID, ok := r.venueAPIKeys[apiKey]
	return venueID, ok, nil
}

func (r *Repository) ListVenues(_ context.Context, city string, cursor *repository.VenueCursor, limit int32) ([]platformapi.Venue, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]platformapi.Venue, 0, len(r.venueOrder))
	for _, id := range r.venueOrder {
		venue := r.venues[id]
		if city != "" && !strings.EqualFold(venue.City, city) {
			continue
		}
		if cursor != nil && (venue.Name < cursor.Name || (venue.Name == cursor.Name && venue.Id.String() <= cursor.ID.String())) {
			continue
		}
		items = append(items, venue)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name == items[j].Name {
			return items[i].Id.String() < items[j].Id.String()
		}
		return items[i].Name < items[j].Name
	})
	if int32(len(items)) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (r *Repository) FindVenue(_ context.Context, id uuid.UUID) (platformapi.Venue, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	venue, ok := r.venues[id]
	return venue, ok, nil
}

func (r *Repository) FindMenu(_ context.Context, venueID uuid.UUID) (platformapi.Menu, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	menu, ok := r.menus[venueID]
	return menu, ok, nil
}

func (r *Repository) FindOrderForVenue(_ context.Context, venueID, orderID uuid.UUID) (platformapi.Order, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	order, ok := r.orders[orderID]
	if !ok || order.VenueId != venueID {
		return platformapi.Order{}, false, nil
	}
	return order, ok, nil
}

func (r *Repository) FindOrderForUser(_ context.Context, userID string, orderID uuid.UUID) (platformapi.Order, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.orderUsers[orderID] != userID {
		return platformapi.Order{}, false, nil
	}
	order, ok := r.orders[orderID]
	return order, ok, nil
}

func (r *Repository) ListOrdersForUser(_ context.Context, userID string, cursor *repository.OrderCursor, limit int32) ([]platformapi.Order, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listOrdersLocked(func(id uuid.UUID) bool { return r.orderUsers[id] == userID }, cursor, limit), nil
}

func (r *Repository) ListOrdersForPartner(_ context.Context, venueID uuid.UUID, status string, cursor *repository.OrderCursor, limit int32) ([]platformapi.Order, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listOrdersLocked(func(id uuid.UUID) bool {
		if r.orders[id].VenueId != venueID {
			return false
		}
		if status == "" {
			return r.orders[id].Status == platformapi.OrderStatusPendingConfirmation
		}
		return string(r.orders[id].Status) == status
	}, cursor, limit), nil
}

func (r *Repository) CreateOrderCommand(_ context.Context, userID string, command repository.OrderCommand) (repository.OrderCommandResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := commandKey(command.Subject, command.Operation, command.IdempotencyKey)
	if existing, ok := r.commands[key]; ok {
		if existing.requestHash != command.RequestHash {
			return repository.OrderCommandResult{RequestHashMismatch: true}, nil
		}
		return repository.OrderCommandResult{Order: existing.response}, nil
	}
	menu, found := r.menus[command.Order.VenueId]
	if !found || menu.Version != command.MenuVersion {
		return repository.OrderCommandResult{MenuVersionConflict: true}, nil
	}
	r.orders[command.Order.Id] = command.Order
	r.orderUsers[command.Order.Id] = userID
	r.orderOrder = append(r.orderOrder, command.Order.Id)
	r.nextEventSeq++
	r.events = append(r.events, eventRecord{sequence: r.nextEventSeq, event: command.Event})
	r.commands[key] = commandRecord{response: command.Order, requestHash: command.RequestHash}
	return repository.OrderCommandResult{Order: command.Order}, nil
}

func (r *Repository) ApplyOrderCommand(_ context.Context, command repository.OrderCommand) (repository.OrderCommandResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := commandKey(command.Subject, command.Operation, command.IdempotencyKey)
	if existing, ok := r.commands[key]; ok {
		if existing.requestHash != command.RequestHash {
			return repository.OrderCommandResult{RequestHashMismatch: true}, nil
		}
		return repository.OrderCommandResult{Order: existing.response}, nil
	}
	current, found := r.orders[command.Order.Id]
	if !found || current.Status != command.ExpectedStatus {
		return repository.OrderCommandResult{StateConflict: true}, nil
	}
	r.orders[command.Order.Id] = command.Order
	if command.VenueOrderID != nil {
		r.venueOrderIDs[command.Order.Id] = *command.VenueOrderID
	}
	r.nextEventSeq++
	r.events = append(r.events, eventRecord{sequence: r.nextEventSeq, event: command.Event})
	r.commands[key] = commandRecord{response: command.Order, requestHash: command.RequestHash}
	return repository.OrderCommandResult{Order: command.Order}, nil
}

func (r *Repository) VenueOrderID(_ context.Context, orderID uuid.UUID) (string, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	venueOrderID, ok := r.venueOrderIDs[orderID]
	return venueOrderID, ok, nil
}

func (r *Repository) FindCommand(_ context.Context, subject, operation, key string) (platformapi.Order, string, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, ok := r.commands[commandKey(subject, operation, key)]
	if !ok {
		return platformapi.Order{}, "", false, nil
	}
	return record.response, record.requestHash, true, nil
}

func (r *Repository) ListOrderEvents(_ context.Context, venueID uuid.UUID, afterSequence uint64, limit int32) ([]platformapi.OrderEvent, uint64, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := make([]platformapi.OrderEvent, 0)
	var lastSequence uint64
	for _, record := range r.events {
		if record.sequence <= afterSequence {
			continue
		}
		if record.event.Order.VenueId != venueID {
			continue
		}
		items = append(items, record.event)
		lastSequence = record.sequence
		if int32(len(items)) == limit {
			break
		}
	}
	return items, lastSequence, len(items) > 0, nil
}

func (r *Repository) ProductMappings(_ context.Context, venueID uuid.UUID) (map[string]uuid.UUID, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return copyMappings(r.externalProduct[venueID]), nil
}

func (r *Repository) SaveMenuSnapshot(_ context.Context, venue platformapi.Venue, menu platformapi.Menu, productIDs map[string]uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, found := r.menus[venue.Id]; found && menu.Version <= current.Version {
		return false, nil
	}
	r.venues[venue.Id] = venue
	r.menus[venue.Id] = menu
	r.externalProduct[venue.Id] = copyMappings(productIDs)
	return true, nil
}

func (r *Repository) listOrdersLocked(predicate func(uuid.UUID) bool, cursor *repository.OrderCursor, limit int32) []platformapi.Order {
	items := make([]platformapi.Order, 0)
	for _, id := range r.orderOrder {
		if predicate(id) {
			order := r.orders[id]
			if cursor != nil && (order.CreatedAt.After(cursor.CreatedAt) || (order.CreatedAt.Equal(cursor.CreatedAt) && order.Id.String() >= cursor.ID.String())) {
				continue
			}
			items = append(items, order)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].Id.String() > items[j].Id.String()
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	if int32(len(items)) > limit {
		items = items[:limit]
	}
	return items
}

func copyMappings(source map[string]uuid.UUID) map[string]uuid.UUID {
	result := make(map[string]uuid.UUID, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func stringPtr(value string) *string {
	return &value
}

func commandKey(subject, operation, key string) string {
	return subject + ":" + operation + ":" + key
}

var _ repository.Repository = (*Repository)(nil)
