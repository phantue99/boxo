package internal

import (
	"context"
	"io"
)

type BackUpStorageI interface {
	Upload(ctx context.Context, key string, reader io.Reader) error
	Download(ctx context.Context, key string) (io.Reader, error)
	Delete(ctx context.Context, key string) error
}
