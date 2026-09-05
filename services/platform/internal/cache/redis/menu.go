// Package redis implements the optional public-menu cache.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/service"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

type MenuCache struct {
	client  *goredis.Client
	ttl     time.Duration
	timeout time.Duration
}

func New(rawURL string, ttl, timeout time.Duration) (*MenuCache, error) {
	if ttl <= 0 || timeout <= 0 {
		return nil, errors.New("redis TTL and timeout must be positive")
	}
	options, err := goredis.ParseURL(rawURL)
	if err != nil {
		return nil, errors.New("invalid REDIS_URL")
	}
	options.MaxRetries = -1
	options.DialerRetries = 1
	options.DialTimeout = timeout
	options.ReadTimeout = timeout
	options.WriteTimeout = timeout
	options.PoolTimeout = timeout
	options.ContextTimeoutEnabled = true
	return &MenuCache{client: goredis.NewClient(options), ttl: ttl, timeout: timeout}, nil
}

func (c *MenuCache) Close() error { return c.client.Close() }

func menuKey(venueID uuid.UUID, version int64) string {
	return fmt.Sprintf("mealroute:public-menu:v1:%s:%d", venueID, version)
}

func (c *MenuCache) Get(ctx context.Context, venueID uuid.UUID, version int64) (platformapi.Menu, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	payload, err := c.client.Get(ctx, menuKey(venueID, version)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return platformapi.Menu{}, false, nil
	}
	if err != nil {
		return platformapi.Menu{}, false, err
	}
	var menu platformapi.Menu
	if json.Unmarshal(payload, &menu) != nil || menu.VenueId != venueID || menu.Version != version {
		// Treat invalid data as a miss so the caller replaces it from PostgreSQL.
		return platformapi.Menu{}, false, nil
	}
	return menu, true, nil
}

func (c *MenuCache) Put(ctx context.Context, menu platformapi.Menu) error {
	payload, err := json.Marshal(menu)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.client.Set(ctx, menuKey(menu.VenueId, menu.Version), payload, c.ttl).Err()
}

var _ service.MenuCache = (*MenuCache)(nil)
