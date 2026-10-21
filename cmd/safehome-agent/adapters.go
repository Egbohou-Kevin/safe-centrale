package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/photodir"
	"safehome/internal/adapter/upload"
	"safehome/internal/agentapp"
	"safehome/internal/service"
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

// cloudPublisher fits the cloud's client to the application's port: a message its
// broker refused for good says so.
type cloudPublisher struct{ p *service.Publisher }

func (c cloudPublisher) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	err := c.p.Publish(ctx, topic, payload, qos, retain)
	if errors.Is(err, mqtt.ErrRefused) {
		return fmt.Errorf("%w: %w", agentapp.ErrRefused, err)
	}
	return err
}

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
