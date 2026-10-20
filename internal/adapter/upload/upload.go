// Package upload sends a file to the cloud over HTTPS: one PUT, the token in
// "Authorization: Bearer". It knows nothing of photos nor of the contract. The token
// never appears in an error or a log.
package upload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// The answers that are not worth another try with the same address.
var (
	// ErrExpired: the address or the token are no good (401, 403, 404, 410).
	ErrExpired = errors.New("upload: address refused")
	// ErrRefused: the file itself is refused (400, 413, 415, 422).
	ErrRefused = errors.New("upload: file refused")
)

// Client sends files.
type Client struct {
	http *http.Client
}

// New builds a client. caFile, if set, is the certificate authority of the cloud
// (as for its MQTT broker); otherwise the system's are used.
func New(caFile string) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("upload: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("upload: no certificate in %s", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &Client{http: &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: nil, TLSHandshakeTimeout: 20 * time.Second, MaxIdleConns: 1},
		// No redirection: the token would follow it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Put sends size bytes of body to target (https only), with the token.
func (c *Client) Put(ctx context.Context, target, token string, body io.Reader, size int64) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: not an https address", ErrExpired)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload to %s: %w", u.Host, unwrap(err))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		return nil
	case s == 401 || s == 403 || s == 404 || s == 410:
		return fmt.Errorf("%w (%d)", ErrExpired, s)
	case s == 400 || s == 413 || s == 415 || s == 422:
		return fmt.Errorf("%w (%d)", ErrRefused, s)
	}
	return fmt.Errorf("upload to %s: answer %d", u.Host, resp.StatusCode)
}

// unwrap drops the address the http package repeats in its errors.
func unwrap(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
