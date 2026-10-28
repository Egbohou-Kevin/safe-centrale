package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
// is written with the rights of its owner alone, whole or not at all, and on the card
// before it is used. A key file open to others is made private again. A damaged one (a
// power cut) is set aside and replaced, rather than stop the agent, which carries the
// alarms: the new public half reaches the phones with agent.status, and a seal made for
// the old one is answered bad_seal. A key read or written whole is used even when it
// cannot be made private or its folder flushed (a file left to root, a card that refuses
// a folder sync): that is warned, not fatal. No message quotes the key.
func codeKey(dir string, log *slog.Logger) (*sealcode.Key, error) {
	path := filepath.Join(dir, "code-key")
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return newCodeKey(dir, path, log)
	case err != nil:
		return nil, err
	}
	k, err := sealcode.Parse(string(b))
	if err != nil {
		log.Error("the key for the sealed codes is damaged: set aside, a new one is drawn", "path", path, "err", err)
		if err := os.Rename(path, path+".damaged"); err != nil {
			return nil, err
		}
		return newCodeKey(dir, path, log)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		log.Warn("the key for the sealed codes was open to others: made private", "path", path, "mode", st.Mode().Perm().String())
		if err := chmodFile(path, 0o600); err != nil {
			log.Warn("the key for the sealed codes could not be made private: used as it is", "path", path, "err", err)
		}
	}
	return k, nil
}

// newCodeKey draws a key and writes it at path: a file of its own (0600), flushed to the
// card, then renamed, and the folder flushed too, so that a power cut leaves the old
// state or the whole key.
func newCodeKey(dir, path string, log *slog.Logger) (*sealcode.Key, error) {
	k, err := sealcode.Generate()
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, "code-key-*") // 0600
	if err != nil {
		return nil, err
	}
	_, werr := tmp.WriteString(k.Encode())
	if werr == nil {
		werr = tmp.Sync()
	}
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
	if err := syncFolder(dir); err != nil {
		log.Warn("the folder of the key for the sealed codes could not be flushed: key used", "path", path, "err", err)
	}
	return k, nil
}

// chmodFile and syncFolder are os.Chmod and syncDir, replaced in tests to make them fail.
var (
	chmodFile  = os.Chmod
	syncFolder = syncDir
)

// syncDir flushes a folder, so that a file renamed into it stays there after a power cut.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
