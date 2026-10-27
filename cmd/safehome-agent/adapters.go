package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/photodir"
	"safehome/internal/adapter/upload"
	"safehome/internal/agentapp"
	"safehome/internal/sealcode"
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

// codeKey reads the agent's key for the sealed codes, or draws one at the first start. It
// is written with the rights of its owner alone, whole or not at all.
func codeKey(dir string) (*sealcode.Key, error) {
	path := filepath.Join(dir, "code-key")
	b, err := os.ReadFile(path)
	if err == nil {
		return sealcode.Parse(string(b))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, err := sealcode.Generate()
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, "code-key-*") // 0600
	if err != nil {
		return nil, err
	}
	_, werr := tmp.WriteString(k.Encode())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return nil, werr
	}
	return k, nil
}
