// Package postgres implements the platform persistence port with PostgreSQL.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Repository, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return &Repository{pool: pool}, nil
}

func (r *Repository) Close() {
	r.pool.Close()
}

func (r *Repository) ConfigureVenueAPIKey(ctx context.Context, venueID uuid.UUID, apiKey string) error {
	// Keep the bootstrap key identity stable so starting a newer build against an
	// existing volume rotates the original key instead of leaving it active.
	keyID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	keyFingerprint := sha256.Sum256([]byte(apiKey))
	_, err := r.pool.Exec(ctx, `
		INSERT INTO venue_api_keys (id, venue_id, key_fingerprint, revoked_at)
		VALUES ($1, $2, $3, NULL)
		ON CONFLICT (id) DO UPDATE
		SET venue_id = EXCLUDED.venue_id,
		    key_fingerprint = EXCLUDED.key_fingerprint,
		    revoked_at = NULL`, keyID, venueID, hex.EncodeToString(keyFingerprint[:]))
	if err != nil {
		return fmt.Errorf("configure venue API key fingerprint: %w", err)
	}
	return nil
}

func (r *Repository) FindVenueIDByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	keyFingerprint := sha256.Sum256([]byte(apiKey))
	err := r.pool.QueryRow(ctx, `
		SELECT venue_id
		FROM venue_api_keys
		WHERE key_fingerprint = $1 AND revoked_at IS NULL`, hex.EncodeToString(keyFingerprint[:])).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("find venue by API key: %w", err)
	}
	return id, true, nil
}

func (r *Repository) ListVenues(ctx context.Context, city string, cursor *repository.VenueCursor, limit int32) ([]platformapi.Venue, error) {
	query := `
		SELECT id, name, kind, city, address, status, is_open, menu_version, updated_at
		FROM venues
		WHERE ($1 = '' OR city ILIKE $1)
		ORDER BY name, id
		LIMIT $2`
	args := []any{city, limit}
	if cursor != nil {
		query = `
			SELECT id, name, kind, city, address, status, is_open, menu_version, updated_at
			FROM venues
			WHERE ($1 = '' OR city ILIKE $1)
			  AND (name > $2 OR (name = $2 AND id > $3))
			ORDER BY name, id
			LIMIT $4`
		args = []any{city, cursor.Name, cursor.ID, limit}
	}
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list venues: %w", err)
	}
	defer rows.Close()

	venues := make([]platformapi.Venue, 0)
	for rows.Next() {
		venue, err := scanVenue(rows)
		if err != nil {
			return nil, err
		}
		venues = append(venues, venue)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate venues: %w", err)
	}
	return venues, nil
}

func (r *Repository) FindVenue(ctx context.Context, id uuid.UUID) (platformapi.Venue, bool, error) {
	venue, err := scanVenue(r.pool.QueryRow(ctx, `
		SELECT id, name, kind, city, address, status, is_open, menu_version, updated_at
		FROM venues WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return platformapi.Venue{}, false, nil
	}
	if err != nil {
		return platformapi.Venue{}, false, err
	}
	return venue, true, nil
}

func (r *Repository) FindMenu(ctx context.Context, venueID uuid.UUID) (platformapi.Menu, bool, error) {
	var payload []byte
	err := r.pool.QueryRow(ctx, `SELECT payload FROM menus WHERE venue_id = $1`, venueID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return platformapi.Menu{}, false, nil
	}
	if err != nil {
		return platformapi.Menu{}, false, fmt.Errorf("select menu: %w", err)
	}
	menu, err := decodeMenu(payload)
	if err != nil {
		return platformapi.Menu{}, false, err
	}
	return menu, true, nil
}

func (r *Repository) FindOrderForUser(ctx context.Context, userID string, orderID uuid.UUID) (platformapi.Order, bool, error) {
	return r.findOrder(ctx, `SELECT payload FROM orders WHERE id = $1 AND customer_id = $2`, orderID, userID)
}

func (r *Repository) FindOrderForVenue(ctx context.Context, venueID, orderID uuid.UUID) (platformapi.Order, bool, error) {
	return r.findOrder(ctx, `SELECT payload FROM orders WHERE id = $1 AND venue_id = $2`, orderID, venueID)
}

func (r *Repository) ListOrdersForUser(ctx context.Context, userID string, cursor *repository.OrderCursor, limit int32) ([]platformapi.Order, error) {
	if cursor == nil {
		return r.listOrders(ctx, `
			SELECT payload FROM orders
			WHERE customer_id = $1
			ORDER BY created_at DESC, id DESC
			LIMIT $2`, userID, limit)
	}
	return r.listOrders(ctx, `
		SELECT payload FROM orders
		WHERE customer_id = $1
		  AND (created_at < $2 OR (created_at = $2 AND id < $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $4`, userID, cursor.CreatedAt, cursor.ID, limit)
}

func (r *Repository) ListOrdersForPartner(ctx context.Context, venueID uuid.UUID, status string, cursor *repository.OrderCursor, limit int32) ([]platformapi.Order, error) {
	if cursor == nil {
		return r.listOrders(ctx, `
			SELECT payload FROM orders
			WHERE venue_id = $1
			  AND (($2 = '' AND status = 'pending_confirmation') OR ($2 <> '' AND status = $2))
			ORDER BY created_at DESC, id DESC
			LIMIT $3`, venueID, status, limit)
	}
	return r.listOrders(ctx, `
		SELECT payload FROM orders
		WHERE venue_id = $1
		  AND (($2 = '' AND status = 'pending_confirmation') OR ($2 <> '' AND status = $2))
		  AND (created_at < $3 OR (created_at = $3 AND id < $4))
		ORDER BY created_at DESC, id DESC
		LIMIT $5`, venueID, status, cursor.CreatedAt, cursor.ID, limit)
}

func (r *Repository) CreateOrderCommand(ctx context.Context, userID string, command repository.OrderCommand) (repository.OrderCommandResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("begin create order command: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if result, found, err := findCommandTx(ctx, tx, command); err != nil || found {
		return result, err
	}
	var currentMenuVersion int64
	err = tx.QueryRow(ctx, `SELECT menu_version FROM venues WHERE id = $1 FOR SHARE`, command.Order.VenueId).Scan(&currentMenuVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.OrderCommandResult{MenuVersionConflict: true}, nil
	}
	if err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("lock menu version for order: %w", err)
	}
	if currentMenuVersion != command.MenuVersion {
		return repository.OrderCommandResult{MenuVersionConflict: true}, nil
	}
	payload, err := json.Marshal(command.Order)
	if err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("encode order: %w", err)
	}
	insert, err := tx.Exec(ctx, `
		INSERT INTO orders (
			id, venue_id, customer_id, status, payload, created_at, updated_at, create_idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (customer_id, create_idempotency_key) WHERE create_idempotency_key IS NOT NULL DO NOTHING`,
		command.Order.Id, command.Order.VenueId, userID, string(command.Order.Status), payload,
		command.Order.CreatedAt, command.Order.UpdatedAt, command.IdempotencyKey)
	if err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("insert order: %w", err)
	}
	if insert.RowsAffected() == 0 {
		result, found, err := findCommandTx(ctx, tx, command)
		if err != nil {
			return repository.OrderCommandResult{}, err
		}
		if found {
			return result, nil
		}
		return repository.OrderCommandResult{}, fmt.Errorf("idempotent order exists without command record")
	}
	if err := storeOrderDetails(ctx, tx, command.Order); err != nil {
		return repository.OrderCommandResult{}, err
	}
	if err := appendOrderEventTx(ctx, tx, command.Event); err != nil {
		return repository.OrderCommandResult{}, err
	}
	if err := saveCommandTx(ctx, tx, command); err != nil {
		return repository.OrderCommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("commit create order command: %w", err)
	}
	return repository.OrderCommandResult{Order: command.Order}, nil
}

func (r *Repository) ApplyOrderCommand(ctx context.Context, command repository.OrderCommand) (repository.OrderCommandResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("begin order command: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1 FOR UPDATE`, command.Order.Id).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.OrderCommandResult{StateConflict: true}, nil
	}
	if err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("lock order for command: %w", err)
	}
	if result, found, err := findCommandTx(ctx, tx, command); err != nil || found {
		return result, err
	}
	if currentStatus != string(command.ExpectedStatus) {
		return repository.OrderCommandResult{StateConflict: true}, nil
	}

	payload, err := json.Marshal(command.Order)
	if err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("encode updated order: %w", err)
	}
	var venueOrderID any
	if command.VenueOrderID != nil {
		venueOrderID = *command.VenueOrderID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE orders
		SET status = $2, payload = $3, updated_at = $4, venue_order_id = COALESCE($5, venue_order_id)
		WHERE id = $1`, command.Order.Id, string(command.Order.Status), payload, command.Order.UpdatedAt, venueOrderID); err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("update order for command: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM order_items WHERE order_id = $1`, command.Order.Id); err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("clear order items: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM order_status_history WHERE order_id = $1`, command.Order.Id); err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("clear order status history: %w", err)
	}
	if err := storeOrderDetails(ctx, tx, command.Order); err != nil {
		return repository.OrderCommandResult{}, err
	}
	if err := appendOrderEventTx(ctx, tx, command.Event); err != nil {
		return repository.OrderCommandResult{}, err
	}
	if err := saveCommandTx(ctx, tx, command); err != nil {
		return repository.OrderCommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return repository.OrderCommandResult{}, fmt.Errorf("commit order command: %w", err)
	}
	return repository.OrderCommandResult{Order: command.Order}, nil
}

func (r *Repository) VenueOrderID(ctx context.Context, orderID uuid.UUID) (string, bool, error) {
	var venueOrderID *string
	err := r.pool.QueryRow(ctx, `SELECT venue_order_id FROM orders WHERE id = $1`, orderID).Scan(&venueOrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("select venue order id: %w", err)
	}
	if venueOrderID == nil {
		return "", false, nil
	}
	return *venueOrderID, true, nil
}

func (r *Repository) FindCommand(ctx context.Context, subject, operation, key string) (platformapi.Order, string, bool, error) {
	var payload []byte
	var requestHash string
	err := r.pool.QueryRow(ctx, `
		SELECT response_payload, request_hash
		FROM idempotency_commands AS c
		WHERE c.subject = $1 AND c.operation = $2 AND c.idempotency_key = $3`, subject, operation, key).
		Scan(&payload, &requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return platformapi.Order{}, "", false, nil
	}
	if err != nil {
		return platformapi.Order{}, "", false, fmt.Errorf("find idempotency command: %w", err)
	}
	order, err := decodeOrder(payload)
	if err != nil {
		return platformapi.Order{}, "", false, err
	}
	return order, requestHash, true, nil
}

func findCommandTx(ctx context.Context, tx pgx.Tx, command repository.OrderCommand) (repository.OrderCommandResult, bool, error) {
	var payload []byte
	var requestHash string
	err := tx.QueryRow(ctx, `
		SELECT response_payload, request_hash
		FROM idempotency_commands
		WHERE subject = $1 AND operation = $2 AND idempotency_key = $3`,
		command.Subject, command.Operation, command.IdempotencyKey).Scan(&payload, &requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.OrderCommandResult{}, false, nil
	}
	if err != nil {
		return repository.OrderCommandResult{}, false, fmt.Errorf("find command in transaction: %w", err)
	}
	if requestHash != command.RequestHash {
		return repository.OrderCommandResult{RequestHashMismatch: true}, true, nil
	}
	order, err := decodeOrder(payload)
	if err != nil {
		return repository.OrderCommandResult{}, false, err
	}
	return repository.OrderCommandResult{Order: order}, true, nil
}

func saveCommandTx(ctx context.Context, tx pgx.Tx, command repository.OrderCommand) error {
	payload, err := json.Marshal(command.Order)
	if err != nil {
		return fmt.Errorf("encode idempotency response: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO idempotency_commands (
			subject, operation, idempotency_key, request_hash, response_payload, order_id
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		command.Subject, command.Operation, command.IdempotencyKey, command.RequestHash,
		payload, command.Order.Id)
	if err != nil {
		return fmt.Errorf("save idempotency command: %w", err)
	}
	return nil
}

func appendOrderEventTx(ctx context.Context, tx pgx.Tx, event platformapi.OrderEvent) error {
	payload, err := json.Marshal(event.Order)
	if err != nil {
		return fmt.Errorf("encode order event: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO order_events (id, venue_id, order_id, event_type, order_snapshot, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		event.EventId, event.Order.VenueId, event.Order.Id, string(event.Type), payload, event.OccurredAt)
	if err != nil {
		return fmt.Errorf("append order event: %w", err)
	}
	return nil
}

func (r *Repository) ListOrderEvents(ctx context.Context, venueID uuid.UUID, afterSequence uint64, limit int32) ([]platformapi.OrderEvent, uint64, bool, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT sequence_no, id, event_type, order_snapshot, occurred_at
		FROM order_events
		WHERE venue_id = $1 AND sequence_no > $2
		ORDER BY sequence_no
		LIMIT $3`, venueID, afterSequence, limit)
	if err != nil {
		return nil, 0, false, fmt.Errorf("list order events: %w", err)
	}
	defer rows.Close()

	events := make([]platformapi.OrderEvent, 0)
	var lastSequence uint64
	for rows.Next() {
		var sequence uint64
		var eventID uuid.UUID
		var eventType string
		var payload []byte
		var occurredAt time.Time
		if err := rows.Scan(&sequence, &eventID, &eventType, &payload, &occurredAt); err != nil {
			return nil, 0, false, fmt.Errorf("scan order event: %w", err)
		}
		order, err := decodeOrder(payload)
		if err != nil {
			return nil, 0, false, err
		}
		events = append(events, platformapi.OrderEvent{EventId: eventID, Type: platformapi.OrderEventType(eventType), OccurredAt: occurredAt, Order: order})
		lastSequence = sequence
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("iterate order events: %w", err)
	}
	return events, lastSequence, len(events) > 0, nil
}

func (r *Repository) ProductMappings(ctx context.Context, venueID uuid.UUID) (map[string]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx, `SELECT external_item_id, id FROM menu_items WHERE venue_id = $1`, venueID)
	if err != nil {
		return nil, fmt.Errorf("list product mappings: %w", err)
	}
	defer rows.Close()
	mappings := make(map[string]uuid.UUID)
	for rows.Next() {
		var externalID string
		var productID uuid.UUID
		if err := rows.Scan(&externalID, &productID); err != nil {
			return nil, fmt.Errorf("scan product mapping: %w", err)
		}
		mappings[externalID] = productID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate product mappings: %w", err)
	}
	return mappings, nil
}

func (r *Repository) SaveMenuSnapshot(ctx context.Context, venue platformapi.Venue, menu platformapi.Menu, productIDs map[string]uuid.UUID) (bool, error) {
	payload, err := json.Marshal(menu)
	if err != nil {
		return false, fmt.Errorf("encode menu: %w", err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin save menu: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT menu_version FROM venues WHERE id = $1 FOR UPDATE`, venue.Id).Scan(&currentVersion)
	if err != nil {
		return false, fmt.Errorf("lock venue menu version: %w", err)
	}
	if menu.Version <= currentVersion {
		return false, nil
	}
	_, err = tx.Exec(ctx, `
		UPDATE venues SET menu_version = $2, updated_at = $3 WHERE id = $1`, venue.Id, venue.MenuVersion, venue.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("update venue menu version: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO menus (venue_id, version, currency, payload, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (venue_id) DO UPDATE
		SET version = EXCLUDED.version, currency = EXCLUDED.currency, payload = EXCLUDED.payload, updated_at = EXCLUDED.updated_at`,
		menu.VenueId, menu.Version, string(menu.Currency), payload, menu.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("upsert menu: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM menu_categories WHERE venue_id = $1`, venue.Id); err != nil {
		return false, fmt.Errorf("clear menu categories: %w", err)
	}
	for _, category := range menu.Categories {
		sortOrder := int32(0)
		if category.SortOrder != nil {
			sortOrder = *category.SortOrder
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO menu_categories (id, venue_id, external_category_id, name, sort_order)
			VALUES ($1, $2, $3, $4, $5)`,
			category.Id, venue.Id, category.Id.String(), category.Name, sortOrder); err != nil {
			return false, fmt.Errorf("insert menu category: %w", err)
		}
		for _, item := range category.Items {
			sortOrder := int32(0)
			if item.SortOrder != nil {
				sortOrder = *item.SortOrder
			}
			externalID, ok := externalIDForProduct(productIDs, item.Id)
			if !ok {
				return false, fmt.Errorf("missing external item mapping for product %s", item.Id)
			}
			tags := []string{}
			if item.Tags != nil {
				tags = *item.Tags
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO menu_items (
					id, venue_id, category_id, external_item_id, name, description,
					price_amount, currency, available, sort_order, tags
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				item.Id, venue.Id, category.Id, externalID, item.Name, item.Description,
				item.Price.Amount, string(item.Price.Currency), item.Available, sortOrder, tags); err != nil {
				return false, fmt.Errorf("insert menu item: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit save menu: %w", err)
	}
	return true, nil
}

func (r *Repository) findOrder(ctx context.Context, query string, args ...any) (platformapi.Order, bool, error) {
	var payload []byte
	err := r.pool.QueryRow(ctx, query, args...).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return platformapi.Order{}, false, nil
	}
	if err != nil {
		return platformapi.Order{}, false, fmt.Errorf("select order: %w", err)
	}
	order, err := decodeOrder(payload)
	if err != nil {
		return platformapi.Order{}, false, err
	}
	return order, true, nil
}

func (r *Repository) listOrders(ctx context.Context, query string, args ...any) ([]platformapi.Order, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()
	orders := make([]platformapi.Order, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		order, err := decodeOrder(payload)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orders: %w", err)
	}
	return orders, nil
}

type venueScanner interface {
	Scan(dest ...any) error
}

func scanVenue(row venueScanner) (platformapi.Venue, error) {
	var venue platformapi.Venue
	var kind string
	var status string
	if err := row.Scan(
		&venue.Id, &venue.Name, &kind, &venue.City, &venue.Address,
		&status, &venue.IsOpen, &venue.MenuVersion, &venue.UpdatedAt,
	); err != nil {
		return platformapi.Venue{}, err
	}
	venue.Kind = platformapi.VenueKind(kind)
	venue.Status = platformapi.VenueStatus(status)
	return venue, nil
}

type orderDetailsExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func storeOrderDetails(ctx context.Context, tx orderDetailsExecutor, order platformapi.Order) error {
	for index, item := range order.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_items (
				order_id, line_no, product_id, name, quantity,
				unit_price_amount, total_price_amount, currency
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			order.Id, index+1, item.ProductId, item.Name, item.Quantity,
			item.UnitPrice.Amount, item.TotalPrice.Amount, string(item.UnitPrice.Currency)); err != nil {
			return fmt.Errorf("insert order item: %w", err)
		}
	}
	if order.StatusHistory == nil {
		return nil
	}
	for index, change := range *order.StatusHistory {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_status_history (order_id, sequence_no, status, source, reason, changed_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			order.Id, index+1, string(change.Status), string(change.Source), change.Reason, change.ChangedAt); err != nil {
			return fmt.Errorf("insert order status history: %w", err)
		}
	}
	return nil
}

func decodeMenu(payload []byte) (platformapi.Menu, error) {
	var menu platformapi.Menu
	if err := json.Unmarshal(payload, &menu); err != nil {
		return platformapi.Menu{}, fmt.Errorf("decode stored menu: %w", err)
	}
	return menu, nil
}

func decodeOrder(payload []byte) (platformapi.Order, error) {
	var order platformapi.Order
	if err := json.Unmarshal(payload, &order); err != nil {
		return platformapi.Order{}, fmt.Errorf("decode stored order: %w", err)
	}
	return order, nil
}

func externalIDForProduct(productIDs map[string]uuid.UUID, productID uuid.UUID) (string, bool) {
	for externalID, id := range productIDs {
		if id == productID {
			return externalID, true
		}
	}
	return "", false
}

var _ repository.Repository = (*Repository)(nil)
