package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"safehome/internal/adapter/photodir"
	"safehome/internal/adapter/upload"
	"safehome/internal/agentapp"
)

// photos fits the photos' folder to the application's port.
type photos struct{ d *photodir.Dir }

func (p photos) Open(id string) (io.ReadCloser, int64, error) {
	f, n, err := p.d.Open(id)
	if errors.Is(err, photodir.ErrNotFound) {
		return nil, 0, fmt.Errorf("%w: %w", agentapp.ErrNoPhoto, err)
	}
	return f, n, err
}

func (p photos) Remove(id string) error { return p.d.Remove(id) }

// uploader fits HTTPS to the application's port.
type uploader struct{ c *upload.Client }

func (u uploader) Put(ctx context.Context, url, token string, body io.Reader, size int64) error {
	err := u.c.Put(ctx, url, token, body, size)
	switch {
	case errors.Is(err, upload.ErrExpired):
		return fmt.Errorf("%w: %w", agentapp.ErrUploadExpired, err)
	case errors.Is(err, upload.ErrRefused):
		return fmt.Errorf("%w: %w", agentapp.ErrUploadRefused, err)
	}
	return err
}
