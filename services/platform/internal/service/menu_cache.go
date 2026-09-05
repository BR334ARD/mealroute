package service

import (
	"context"

	platformapi "mealroute/platform/internal/api/platform"
	"mealroute/platform/internal/repository"

	"github.com/google/uuid"
)

// MenuCache stores public menu snapshots by venue and immutable menu version.
// Orders and partner commands must always use the authoritative repository.
type MenuCache interface {
	Get(context.Context, uuid.UUID, int64) (platformapi.Menu, bool, error)
	Put(context.Context, platformapi.Menu) error
}

func NewWithMenuCache(store repository.Repository, cache MenuCache) *Service {
	application := New(store)
	application.menuCache = cache
	return application
}
