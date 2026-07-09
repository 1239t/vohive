package repo

import (
	"context"

	"github.com/1239t/vohive/internal/config"
)

type ProxyInstanceRepository interface {
	List(ctx context.Context) ([]config.ProxyInstance, error)
	Get(ctx context.Context, id string) (*config.ProxyInstance, error)
	ReplaceAll(ctx context.Context, instances []config.ProxyInstance) error
}
