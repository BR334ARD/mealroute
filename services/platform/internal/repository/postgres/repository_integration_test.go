package postgres_test

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/domain"
	"mealroute/platform/internal/repository"
	"mealroute/platform/internal/repository/postgres"
	"mealroute/platform/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConcurrentOrderCommandsAreAtomicWithPostgreSQL(t *testing.T) {
	if os.Getenv("POSTGRES_INTEGRATION") != "1" {
		t.Skip("set POSTGRES_INTEGRATION=1 to run PostgreSQL integration tests")
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required when POSTGRES_INTEGRATION=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	if err := admin.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	venueID := uuid.New()
	categoryID := uuid.New()
	productID := uuid.New()
	userID := "postgres-integration-" + uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	menu := platformapi.Menu{
		VenueId:   venueID,
		Version:   1,
		Currency:  platformapi.MenuCurrencyRUB,
		UpdatedAt: now,
		Categories: []platformapi.MenuCategory{{
			Id:   categoryID,
			Name: "Интеграционная категория",
			Items: []platformapi.MenuItem{{
				Id:        productID,
				Name:      "Интеграционное блюдо",
				Price:     platformapi.Money{Amount: 10000, Currency: platformapi.MoneyCurrencyRUB},
				Available: true,
			}},
		}},
	}
	menuPayload, err := json.Marshal(menu)
	if err != nil {
		t.Fatalf("marshal menu: %v", err)
	}
	if _, err := admin.Exec(ctx, `
		INSERT INTO venues (id, name, kind, city, address, status, is_open, menu_version, updated_at)
		VALUES ($1, 'PostgreSQL integration venue', 'restaurant', 'Новосибирск', 'Тестовая, 1', 'active', true, 1, $2)`, venueID, now); err != nil {
		t.Fatalf("insert venue; migrations may be missing: %v", err)
	}
	if _, err := admin.Exec(ctx, `
		INSERT INTO menus (venue_id, version, currency, payload, updated_at)
		VALUES ($1, 1, 'RUB', $2, $3)`, venueID, menuPayload, now); err != nil {
		t.Fatalf("insert menu: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		statements := []string{
			`DELETE FROM order_events WHERE venue_id = $1`,
			`DELETE FROM idempotency_commands WHERE order_id IN (SELECT id FROM orders WHERE venue_id = $1)`,
			`DELETE FROM orders WHERE venue_id = $1`,
			`DELETE FROM menus WHERE venue_id = $1`,
			`DELETE FROM venues WHERE id = $1`,
		}
		for _, statement := range statements {
			if _, err := admin.Exec(cleanupCtx, statement, venueID); err != nil {
				t.Errorf("cleanup PostgreSQL fixture: %v", err)
			}
		}
	})

	store, err := postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(store.Close)
	application := service.New(store)

	created, apiError := application.CreateOrder(ctx, userID, "create", platformapi.CreateOrderRequest{
		VenueId:     venueID,
		MenuVersion: 1,
		Items:       []platformapi.OrderItemInput{{ProductId: productID, Quantity: 1}},
		DeliveryAddress: platformapi.DeliveryAddress{
			City:        "Новосибирск",
			AddressLine: "PostgreSQL-тест, 1",
		},
	})
	if apiError != nil {
		t.Fatalf("create order: %v", apiError)
	}
	if _, apiError = application.AcceptPartnerOrder(ctx, venueID, created.Id, "accept", platformapi.AcceptOrderRequest{VenueOrderId: "integration-order"}); apiError != nil {
		t.Fatalf("accept order: %v", apiError)
	}

	type commandResult struct {
		order platformapi.Order
		err   *domain.Error
	}
	start := make(chan struct{})
	results := make(chan commandResult, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		order, commandError := application.CancelCustomerOrder(ctx, userID, created.Id, "cancel", nil)
		results <- commandResult{order: order, err: commandError}
	}()
	go func() {
		defer wait.Done()
		<-start
		order, commandError := application.UpdatePartnerOrderStatus(ctx, venueID, created.Id, "prepare", platformapi.UpdateOrderStatusRequest{Status: platformapi.UpdateOrderStatusRequestStatusPreparing})
		results <- commandResult{order: order, err: commandError}
	}()
	close(start)
	wait.Wait()
	close(results)

	successes := 0
	conflicts := 0
	var committedStatus platformapi.OrderStatus
	for result := range results {
		if result.err == nil {
			successes++
			committedStatus = result.order.Status
			continue
		}
		if result.err.Code == "invalid_order_transition" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected command error: %v", result.err)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("expected one commit and one conflict, successes=%d conflicts=%d", successes, conflicts)
	}

	stored, apiError := application.GetCustomerOrder(ctx, userID, created.Id)
	if apiError != nil {
		t.Fatalf("get stored order: %v", apiError)
	}
	if stored.Status != committedStatus {
		t.Fatalf("stored status %s differs from committed status %s", stored.Status, committedStatus)
	}
	events, apiError := application.ListPartnerOrderEvents(ctx, venueID, "", 20)
	if apiError != nil {
		t.Fatalf("list events: %v", apiError)
	}
	if len(events.Items) != 3 || events.Items[2].Order.Status != committedStatus {
		t.Fatalf("expected atomic create, accept and winning command events: %+v", events.Items)
	}
}

func TestMenuSyncMakesPausedStaleOrderFailAtomically(t *testing.T) {
	if os.Getenv("POSTGRES_INTEGRATION") != "1" {
		t.Skip("set POSTGRES_INTEGRATION=1 to run PostgreSQL integration tests")
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required when POSTGRES_INTEGRATION=1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	venueID := uuid.New()
	productID := uuid.New()
	categoryID := uuid.New()
	userID := "menu-race-" + uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	oldMenu := platformapi.Menu{
		VenueId: venueID, Version: 1, Currency: platformapi.MenuCurrencyRUB, UpdatedAt: now,
		Categories: []platformapi.MenuCategory{{Id: categoryID, Name: "Старая категория", Items: []platformapi.MenuItem{{Id: productID, Name: "Старая цена", Price: platformapi.Money{Amount: 10000, Currency: platformapi.MoneyCurrencyRUB}, Available: true}}}},
	}
	menuPayload, err := json.Marshal(oldMenu)
	if err != nil {
		t.Fatalf("marshal menu: %v", err)
	}
	if _, err := admin.Exec(ctx, `
		INSERT INTO venues (id, name, kind, city, address, status, is_open, menu_version, updated_at)
		VALUES ($1, 'Menu race venue', 'restaurant', 'Новосибирск', 'Тестовая, 2', 'active', true, 1, $2)`, venueID, now); err != nil {
		t.Fatalf("insert venue: %v", err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO menus (venue_id, version, currency, payload, updated_at) VALUES ($1, 1, 'RUB', $2, $3)`, venueID, menuPayload, now); err != nil {
		t.Fatalf("insert menu: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		statements := []string{
			`DELETE FROM order_events WHERE venue_id = $1`,
			`DELETE FROM idempotency_commands WHERE order_id IN (SELECT id FROM orders WHERE venue_id = $1)`,
			`DELETE FROM orders WHERE venue_id = $1`,
			`DELETE FROM menus WHERE venue_id = $1`,
			`DELETE FROM venues WHERE id = $1`,
		}
		for _, statement := range statements {
			if _, err := admin.Exec(cleanupCtx, statement, venueID); err != nil {
				t.Errorf("cleanup menu race fixture: %v", err)
			}
		}
	})

	store, err := postgres.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(store.Close)
	menuRead := make(chan struct{})
	resumeOrder := make(chan struct{})
	paused := &pauseAfterMenuReadRepository{Repository: store, menuRead: menuRead, resume: resumeOrder}
	orderApplication := service.New(paused)
	menuApplication := service.New(store)

	orderErrors := make(chan *domain.Error, 1)
	go func() {
		_, apiError := orderApplication.CreateOrder(ctx, userID, "stale-menu", platformapi.CreateOrderRequest{
			VenueId: venueID, MenuVersion: 1,
			Items:           []platformapi.OrderItemInput{{ProductId: productID, Quantity: 1}},
			DeliveryAddress: platformapi.DeliveryAddress{City: "Новосибирск", AddressLine: "Тестовая, 2"},
		})
		orderErrors <- apiError
	}()

	select {
	case <-menuRead:
	case <-ctx.Done():
		t.Fatal("order did not reach the paused menu read")
	}
	_, syncError := menuApplication.SyncPartnerMenu(ctx, venueID, platformapi.MenuSyncRequest{
		MenuVersion: 2,
		Categories: []platformapi.MenuCategoryInput{{
			ExternalCategoryId: "new-category", Name: "Новая категория",
			Items: []platformapi.MenuItemInput{{ExternalItemId: "new-item", Name: "Новая цена", Price: platformapi.Money{Amount: 20000, Currency: platformapi.MoneyCurrencyRUB}, Available: true}},
		}},
	})
	close(resumeOrder)
	if syncError != nil {
		t.Fatalf("sync newer menu: %v", syncError)
	}
	select {
	case apiError := <-orderErrors:
		if apiError == nil || apiError.Code != "menu_version_mismatch" {
			t.Fatalf("expected atomic menu_version_mismatch, got %v", apiError)
		}
	case <-ctx.Done():
		t.Fatal("paused order did not finish")
	}
	var orderCount int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM orders WHERE customer_id = $1`, userID).Scan(&orderCount); err != nil {
		t.Fatalf("count stale orders: %v", err)
	}
	if orderCount != 0 {
		t.Fatalf("stale order was committed: count=%d", orderCount)
	}
}

type pauseAfterMenuReadRepository struct {
	repository.Repository
	menuRead chan struct{}
	resume   chan struct{}
	once     sync.Once
}

func (r *pauseAfterMenuReadRepository) FindMenu(ctx context.Context, venueID uuid.UUID) (platformapi.Menu, bool, error) {
	menu, found, err := r.Repository.FindMenu(ctx, venueID)
	if err == nil && found {
		r.once.Do(func() {
			close(r.menuRead)
			<-r.resume
		})
	}
	return menu, found, err
}
