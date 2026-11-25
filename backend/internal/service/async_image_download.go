package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const asyncImageContentMaxBytes int64 = 20 << 20
const asyncImageDownloadConcurrent = 4

func validateAsyncImageDownloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, imageLocalError(502, "image_download_url_rejected", "Image source must use public HTTPS without credentials and port must be 443")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !videoPublicIP(ip) {
		return nil, imageLocalError(502, "image_download_url_rejected", "Image source IP is not public")
	}
	return u, nil
}

func (s *OpenAIGatewayService) asyncImageDownloadClient() (*http.Client, func()) {
	transport := &http.Transport{Proxy: nil, DisableCompression: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, errors.New("Image source connection port must be 443")
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("Resolve image source DNS: %w", err)
		}
		if len(addresses) == 0 {
			return nil, errors.New("Image source DNS returned no addresses")
		}
		for _, ip := range addresses {
			if !videoPublicIP(ip) {
				return nil, errors.New("Image source DNS resolved to a non-public IP")
			}
		}
		dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		var failures []error
		for _, ip := range addresses {
			conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return conn, nil
			}
			failures = append(failures, e)
		}
		return nil, fmt.Errorf("Connect image file source: %w", errors.Join(failures...))
	}
	var roundTripper http.RoundTripper = transport
	if s.asyncImageDownloadTransport != nil {
		roundTripper = s.asyncImageDownloadTransport
	}
	client := &http.Client{Transport: roundTripper, Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("Image source exceeded 3 redirects")
		}
		if _, err := validateAsyncImageDownloadURL(req.URL.String()); err != nil {
			return err
		}
		req.Header = make(http.Header)
		req.Header.Set("Accept", "image/png, image/jpeg, image/webp")
		return nil
	}}
	return client, transport.CloseIdleConnections
}

func (s *OpenAIGatewayService) DownloadAsyncImage(ctx context.Context, c *gin.Context, record *ImageTaskRecord) error {
	if record.Status != ImageTaskStatusCompleted || record.Provider == nil || record.Provider.ResultURL == "" {
		return imageLocalError(409, "image_not_downloadable", "Image task has no confirmed completed output")
	}
	s.asyncImageDownloadOnce.Do(func() { s.asyncImageDownloads = make(chan struct{}, asyncImageDownloadConcurrent) })
	select {
	case s.asyncImageDownloads <- struct{}{}:
		defer func() { <-s.asyncImageDownloads }()
	default:
		return imageLocalError(429, "image_download_concurrency_exceeded", "Image download concurrency limit is 4; retry the download later")
	}
	target, err := validateAsyncImageDownloadURL(record.Provider.ResultURL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	controller := http.NewResponseController(c.Writer)
	deadline, _ := ctx.Deadline()
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return imageLocalError(503, "image_download_deadline_error", err.Error())
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	client, closeClient := s.asyncImageDownloadClient()
	defer closeClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return imageLocalError(502, "image_download_error", err.Error())
	}
	req.Header.Set("Accept", "image/png, image/jpeg, image/webp")
	resp, err := client.Do(req)
	if err != nil {
		status := 502
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = 504
		}
		return imageLocalError(status, "image_download_error", "Request image file source: "+safeVideoError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 65537))
		failure := imageProviderError(resp.StatusCode, resp.Header, body, "")
		failure.Fields["stage"] = "image_download"
		if len(body) > 65536 {
			failure.Fields["body_truncated"] = true
		}
		if readErr != nil {
			failure.Fields["body_read_error"] = safeVideoError(readErr)
		}
		return failure
	}
	err = streamAsyncImage(c, resp)
	if err != nil && c.Writer.Written() {
		slog.Warn("image_async.download_interrupted", "task_id", record.ID, "reason", safeVideoError(err))
		panic(http.ErrAbortHandler)
	}
	return err
}

func streamAsyncImage(c *gin.Context, resp *http.Response) error {
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (contentType != "image/png" && contentType != "image/jpeg" && contentType != "image/webp") || resp.Header.Get("Content-Encoding") != "" {
		return imageLocalError(502, "image_output_invalid", "Image source must return an unencoded PNG, JPEG or WebP file")
	}
	expected := resp.ContentLength
	if expected == 0 || expected > asyncImageContentMaxBytes {
		return imageLocalError(502, "image_output_invalid", fmt.Sprintf("Image file length must be 1..%d bytes", asyncImageContentMaxBytes))
	}
	reader := &videoIdleReader{body: resp.Body, idle: 20 * time.Second}
	header := make([]byte, 512)
	n, err := io.ReadFull(reader, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return imageLocalError(502, "image_download_error", "Read image signature: "+safeVideoError(err))
	}
	header = header[:n]
	if n == 0 || http.DetectContentType(header) != contentType {
		return imageLocalError(502, "image_output_invalid", "Image file signature does not match Content-Type")
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	if expected > 0 {
		c.Header("Content-Length", strconv.FormatInt(expected, 10))
	}
	combined := io.MultiReader(bytes.NewReader(header), reader)
	limit := asyncImageContentMaxBytes
	if expected > 0 {
		limit = expected
	}
	written, err := io.CopyBuffer(c.Writer, io.LimitReader(combined, limit), make([]byte, 32<<10))
	if err != nil {
		return fmt.Errorf("Image transfer interrupted: %w", err)
	}
	if expected > 0 && written != expected {
		return fmt.Errorf("Image transfer length mismatch: expected %d, received %d", expected, written)
	}
	var extra [1]byte
	n, err = combined.Read(extra[:])
	if n != 0 || err != io.EOF {
		return fmt.Errorf("Image transfer exceeded limit or source failed: %v", err)
	}
	return nil
}

func redactAsyncImageSecret(body []byte, secret string) []byte {
	if secret != "" {
		body = []byte(strings.ReplaceAll(string(body), secret, "[REDACTED]"))
	}
	return body
}
