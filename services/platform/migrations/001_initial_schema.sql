-- MealRoute MVP: platform is the source of truth for orders and their events.
-- Amounts are stored in kopecks; every timestamp is UTC at the application boundary.

CREATE TABLE venues (
    id UUID PRIMARY KEY,
    name VARCHAR(200) NOT NULL,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('restaurant', 'cafe', 'store')),
    city VARCHAR(100) NOT NULL,
    address VARCHAR(300) NOT NULL,
    status VARCHAR(16) NOT NULL CHECK (status IN ('active', 'suspended')),
    is_open BOOLEAN NOT NULL,
    menu_version BIGINT NOT NULL DEFAULT 0 CHECK (menu_version >= 0),
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX venues_city_active_idx ON venues (city, status, is_open);

CREATE TABLE venue_api_keys (
    id UUID PRIMARY KEY,
    venue_id UUID NOT NULL REFERENCES venues (id) ON DELETE CASCADE,
    key_fingerprint CHAR(64) NOT NULL UNIQUE,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX venue_api_keys_active_venue_idx ON venue_api_keys (venue_id) WHERE revoked_at IS NULL;

CREATE TABLE menus (
    venue_id UUID PRIMARY KEY REFERENCES venues (id) ON DELETE CASCADE,
    version BIGINT NOT NULL CHECK (version >= 0),
    currency CHAR(3) NOT NULL CHECK (currency = 'RUB'),
    payload JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE menu_categories (
    id UUID PRIMARY KEY,
    venue_id UUID NOT NULL REFERENCES venues (id) ON DELETE CASCADE,
    external_category_id VARCHAR(128) NOT NULL,
    name VARCHAR(120) NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0,
    UNIQUE (venue_id, external_category_id)
);

CREATE INDEX menu_categories_venue_sort_idx ON menu_categories (venue_id, sort_order, id);

CREATE TABLE menu_items (
    id UUID PRIMARY KEY,
    venue_id UUID NOT NULL REFERENCES venues (id) ON DELETE CASCADE,
    category_id UUID NOT NULL REFERENCES menu_categories (id) ON DELETE CASCADE,
    external_item_id VARCHAR(128) NOT NULL,
    name VARCHAR(200) NOT NULL,
    description VARCHAR(2000),
    price_amount BIGINT NOT NULL CHECK (price_amount >= 0),
    currency CHAR(3) NOT NULL CHECK (currency = 'RUB'),
    available BOOLEAN NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0,
    tags TEXT[] NOT NULL DEFAULT '{}',
    UNIQUE (venue_id, external_item_id)
);

CREATE INDEX menu_items_public_menu_idx ON menu_items (venue_id, category_id, sort_order, id);

CREATE TABLE orders (
    id UUID PRIMARY KEY,
    venue_id UUID NOT NULL REFERENCES venues (id),
    customer_id VARCHAR(128) NOT NULL,
    create_idempotency_key VARCHAR(255),
    venue_order_id VARCHAR(128),
    status VARCHAR(32) NOT NULL CHECK (status IN (
        'pending_confirmation', 'accepted', 'preparing', 'ready',
        'delivering', 'delivered', 'rejected', 'cancelled'
    )),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX orders_customer_created_idx ON orders (customer_id, created_at DESC, id DESC);
CREATE INDEX orders_venue_status_created_idx ON orders (venue_id, status, created_at DESC, id DESC);
CREATE UNIQUE INDEX orders_customer_create_idempotency_idx
    ON orders (customer_id, create_idempotency_key) WHERE create_idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX orders_venue_order_id_unique_idx
    ON orders (venue_id, venue_order_id) WHERE venue_order_id IS NOT NULL;

CREATE TABLE order_items (
    order_id UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    line_no SMALLINT NOT NULL CHECK (line_no > 0),
    product_id UUID NOT NULL,
    name VARCHAR(200) NOT NULL,
    quantity INTEGER NOT NULL CHECK (quantity > 0),
    unit_price_amount BIGINT NOT NULL CHECK (unit_price_amount >= 0),
    total_price_amount BIGINT NOT NULL CHECK (total_price_amount >= 0),
    currency CHAR(3) NOT NULL CHECK (currency = 'RUB'),
    PRIMARY KEY (order_id, line_no)
);

CREATE TABLE order_status_history (
    order_id UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    sequence_no SMALLINT NOT NULL CHECK (sequence_no > 0),
    status VARCHAR(32) NOT NULL CHECK (status IN (
        'pending_confirmation', 'accepted', 'preparing', 'ready',
        'delivering', 'delivered', 'rejected', 'cancelled'
    )),
    source VARCHAR(16) NOT NULL CHECK (source IN ('customer', 'platform', 'venue')),
    reason VARCHAR(500),
    changed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (order_id, sequence_no)
);

CREATE TABLE idempotency_commands (
    subject VARCHAR(160) NOT NULL,
    operation VARCHAR(160) NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    request_hash CHAR(64) NOT NULL,
    order_id UUID NOT NULL REFERENCES orders (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (subject, operation, idempotency_key)
);

CREATE TABLE order_events (
    sequence_no BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id UUID NOT NULL UNIQUE,
    venue_id UUID NOT NULL REFERENCES venues (id),
    order_id UUID NOT NULL REFERENCES orders (id),
    event_type VARCHAR(32) NOT NULL CHECK (event_type IN (
        'created', 'accepted', 'rejected', 'cancelled', 'status_changed'
    )),
    order_snapshot JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX order_events_venue_sequence_idx ON order_events (venue_id, sequence_no);

-- A deterministic demo venue keeps Docker Compose runnable from a clean volume.
INSERT INTO venues (id, name, kind, city, address, status, is_open, menu_version, updated_at)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    'Демо Пицца', 'restaurant', 'Новосибирск', 'Красный проспект, 1',
    'active', TRUE, 1, now()
)
ON CONFLICT (id) DO NOTHING;

INSERT INTO menus (venue_id, version, currency, payload, updated_at)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    1,
    'RUB',
    '{
      "venueId": "00000000-0000-0000-0000-000000000001",
      "version": 1,
      "currency": "RUB",
      "categories": [{
        "id": "00000000-0000-0000-0000-000000000010",
        "name": "Пицца",
        "sortOrder": 0,
        "items": [
          {
            "id": "00000000-0000-0000-0000-000000000011",
            "name": "Пепперони",
            "description": "Острая салями, моцарелла и томатный соус",
            "price": {"amount": 54900, "currency": "RUB"},
            "available": true,
            "sortOrder": 0
          },
          {
            "id": "00000000-0000-0000-0000-000000000012",
            "name": "Маргарита",
            "description": "Моцарелла, томаты и базилик",
            "price": {"amount": 44900, "currency": "RUB"},
            "available": true,
            "sortOrder": 0
          }
        ]
      }],
      "updatedAt": "2026-01-01T00:00:00Z"
    }'::jsonb,
    '2026-01-01T00:00:00Z'
)
ON CONFLICT (venue_id) DO NOTHING;

INSERT INTO menu_categories (id, venue_id, external_category_id, name, sort_order)
VALUES ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000001', 'pizza', 'Пицца', 0)
ON CONFLICT (id) DO NOTHING;

INSERT INTO menu_items (
    id, venue_id, category_id, external_item_id, name, description,
    price_amount, currency, available, sort_order
)
VALUES
    ('00000000-0000-0000-0000-000000000011', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000010', 'pepperoni', 'Пепперони', 'Острая салями, моцарелла и томатный соус', 54900, 'RUB', TRUE, 0),
    ('00000000-0000-0000-0000-000000000012', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000010', 'margherita', 'Маргарита', 'Моцарелла, томаты и базилик', 44900, 'RUB', TRUE, 0)
ON CONFLICT (id) DO NOTHING;
