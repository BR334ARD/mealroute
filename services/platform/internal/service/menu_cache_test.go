package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/repository"
	"mealroute/platform/internal/repository/memory"
	"mealroute/platform/internal/service"

	"github.com/google/uuid"
)

type countingMenuRepository struct {
	repository.Repository
	reads int
}

func (r *countingMenuRepository) FindMenu(ctx context.Context, id uuid.UUID) (platformapi.Menu, bool, error) {
	r.reads++
	return r.Repository.FindMenu(ctx, id)
}

type fakeMenuCache struct {
	items  map[string]platformapi.Menu
	getErr error
	putErr error
	gets   int
	puts   int
}

func (c *fakeMenuCache) Get(_ context.Context, id uuid.UUID, version int64) (platformapi.Menu, bool, error) {
	c.gets++
	menu, found := c.items[fmt.Sprintf("%s:%d", id, version)]
	return menu, found, c.getErr
}

func (c *fakeMenuCache) Put(_ context.Context, menu platformapi.Menu) error {
	c.puts++
	if c.putErr != nil {
		return c.putErr
	}
	c.items[fmt.Sprintf("%s:%d", menu.VenueId, menu.Version)] = menu
	return nil
}

func TestPublicMenuCacheHitAndVersionInvalidation(t *testing.T) {
	ctx := context.Background()
	store := &countingMenuRepository{Repository: memory.New()}
	cache := &fakeMenuCache{items: make(map[string]platformapi.Menu)}
	app := service.NewWithMenuCache(store, cache)
	id := uuid.MustParse(memory.DemoVenueID)
	old, err := app.GetMenu(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetMenu(ctx, id); err != nil {
		t.Fatal(err)
	}
	if store.reads != 1 {
		t.Fatalf("cache hit read the menu from storage: %d", store.reads)
	}

	// An actual partner command advances the authoritative menu version.
	_, err = app.SyncPartnerMenu(ctx, id, platformapi.MenuSyncRequest{MenuVersion: old.Version + 1, Categories: []platformapi.MenuCategoryInput{}})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a slow reader writing the OLD snapshot after the update committed.
	if err := cache.Put(ctx, old); err != nil {
		t.Fatal(err)
	}
	updated, err := app.GetMenu(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != old.Version+1 || len(updated.Categories) != 0 {
		t.Fatalf("stale cache selected after menu sync: %+v", updated)
	}

	readsBeforeOrder := store.reads
	getsBeforeOrder := cache.gets
	_, err = app.CreateOrder(ctx, "cache-checkout-user", "create", platformapi.CreateOrderRequest{
		VenueId: id, MenuVersion: old.Version,
		Items:           []platformapi.OrderItemInput{{ProductId: old.Categories[0].Items[0].Id, Quantity: 1}},
		DeliveryAddress: platformapi.DeliveryAddress{City: "Новосибирск", AddressLine: "Тест, 1"},
	})
	if err == nil || err.Code != "menu_version_mismatch" {
		t.Fatalf("stale checkout accepted: %v", err)
	}
	if store.reads != readsBeforeOrder+1 || cache.gets != getsBeforeOrder {
		t.Fatal("checkout must read the authoritative menu without consulting cache")
	}
}

func TestPublicMenuCacheFailureFallsBack(t *testing.T) {
	for _, operation := range []string{"get", "put"} {
		t.Run(operation, func(t *testing.T) {
			cache := &fakeMenuCache{items: make(map[string]platformapi.Menu)}
			if operation == "get" {
				cache.getErr = errors.New("offline")
			} else {
				cache.putErr = errors.New("offline")
			}
			app := service.NewWithMenuCache(memory.New(), cache)
			menu, err := app.GetMenu(context.Background(), uuid.MustParse(memory.DemoVenueID))
			if err != nil || len(menu.Categories) == 0 {
				t.Fatalf("fallback failed: %+v, %v", menu, err)
			}
			if operation == "get" && cache.puts != 0 {
				t.Fatal("attempted write after Redis read failure")
			}
		})
	}
}
