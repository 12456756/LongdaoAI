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
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
)

const videoContentMaxBytes int64 = 256 << 20

func videoPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/23", "2002::/16", "64:ff9b::/96", "64:ff9b:1::/48"} {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return false
		}
	}
	return true
}

func validateVideoDownloadURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return nil, videoError(502, "video_download_url_rejected", "Unsupported upstream video download address")
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil && !videoPublicIP(ip) {
		return nil, videoError(502, "video_download_url_rejected", "Video download target is not public")
	}
	return u, nil
}

func (s *OpenAIGatewayService) legacyDownloadClient() (*http.Client, func()) {
	transport := &http.Transport{
		Proxy: nil, DisableCompression: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != "443" {
				return nil, fmt.Errorf("video connection target rejected")
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("Resolve video file source DNS: %w", err)
			}
			if len(addresses) == 0 {
				return nil, fmt.Errorf("Video file source DNS returned no IP addresses")
			}
			for _, ip := range addresses {
				if !videoPublicIP(ip) {
					return nil, fmt.Errorf("video connection target rejected")
				}
			}
			dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
			var dialErrors []error
			for _, ip := range addresses {
				conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if e == nil {
					return conn, nil
				}
				dialErrors = append(dialErrors, e)
			}
			return nil, fmt.Errorf("Connect to video file source: %w", errors.Join(dialErrors...))
		},
	}
	var roundTripper http.RoundTripper = transport
	if s.videoDownloadTransport != nil {
		roundTripper = s.videoDownloadTransport
	}
	return &http.Client{Transport: roundTripper, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return fmt.Errorf("too many video redirects")
		}
		if _, err := validateVideoDownloadURL(req.URL.String()); err != nil {
			return err
		}
		req.Header = make(http.Header)
		req.Header.Set("Accept", "video/mp4, application/octet-stream")
		return nil
	}}, transport.CloseIdleConnections
}

type videoIdleReader struct {
	body io.ReadCloser
	idle time.Duration
}

func (r *videoIdleReader) Read(p []byte) (int, error) {
	var expired atomic.Bool
	timer := time.AfterFunc(r.idle, func() { expired.Store(true); _ = r.body.Close() })
	defer timer.Stop()
	n, err := r.body.Read(p)
	if expired.Load() {
		return n, context.DeadlineExceeded
	}
	return n, err
}

func (s *OpenAIGatewayService) downloadLegacyVideo(ctx context.Context, c *gin.Context, account *Account, key *APIKey, id string) error {
	seconds := s.cfg.VideoLegacy.DownloadTimeoutSeconds
	idle := s.cfg.VideoLegacy.DownloadIdleSeconds
	if seconds < 1 || seconds >= 180 || idle < 1 {
		return videoError(503, "video_configuration_error", "Video download deadlines must be configured below the client timeout")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	controller := http.NewResponseController(c.Writer)
	deadline, _ := ctx.Deadline()
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return videoError(503, "video_download_error", "Video response deadline unavailable")
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	client, closeClient := s.legacyDownloadClient()
	defer closeClient()
	for attempt := 0; attempt < 2; attempt++ {
		task, meta, err := s.queryLegacyVideo(ctx, account, key, id)
		if err != nil {
			return err
		}
		s.contractFromTask(ctx, c, key, meta, "upstream")
		contractFor(c).Billing = s.finishVideoBilling(ctx, key, account, meta, videoQuotaUpdater(ctx))
		c.Header("X-Video-Billing-Status", contractFor(c).Billing.Status)
		if task.Status != "succeeded" || meta.LastStatus != "completed" {
			if task.Status == "failed" {
				failure := legacyTaskFailure(task, account.GetOpenAIApiKey())
				message := fmt.Sprintf("Video generation failed [%s]: %s", failure["code"], failure["message"])
				failure["upstream_message"] = failure["message"]
				failure["message"] = message
				failure["stage"] = "video_generation"
				return &videoProtocolError{status: 409, code: "video_not_downloadable", message: message, diagnostic: failure}
			}
			return videoError(409, "video_not_downloadable", "Video task has no completed output")
		}
		if task.VideoURL == "" {
			return videoError(503, "video_output_unavailable", "Upstream task status=succeeded but its response contains no video_url")
		}
		target, err := validateVideoDownloadURL(task.VideoURL)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return videoError(502, "video_download_error", "Invalid upstream video download request")
		}
		req.Header.Set("Accept", "video/mp4, application/octet-stream")
		resp, err := client.Do(req)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return videoCause(504, "video_download_timeout", "Video download timed out", err)
			}
			return videoCause(502, "video_download_error", "Request video file from source", err)
		}
		if attempt == 0 && (resp.StatusCode == 403 || resp.StatusCode == 404) {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 65536))
			slog.Warn("video_legacy.download_link_refresh", "task_id", id, "status", resp.StatusCode, "reason", errordetail.Message(body), "upstream_request_id", errordetail.RequestID(resp.Header), "read_error", safeVideoError(readErr))
			resp.Body.Close()
			continue
		}
		if resp.StatusCode != 200 {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 65536))
			resp.Body.Close()
			failure := videoUpstreamError(502, "video_download_error", resp.StatusCode, resp.Header, body)
			if readErr != nil {
				failure.(*videoProtocolError).cause = readErr
				failure.(*videoProtocolError).diagnostic["body_read_error"] = safeVideoError(readErr)
			}
			return failure
		}
		err = s.streamLegacyVideo(c, resp, time.Duration(idle)*time.Second)
		resp.Body.Close()
		if err != nil {
			if c.Writer.Written() {
				slog.Warn("video_legacy.download_interrupted", "task_id", id, "account_id", account.ID, "reason", safeVideoError(err))
				// Re-panic through the recovery middleware so net/http emits an incomplete stream.
				panic(http.ErrAbortHandler)
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return videoError(504, "video_download_timeout", "Video download timed out")
			}
		}
		return err
	}
	return videoError(502, "video_download_error", "Video file unavailable after link refresh")
}

func (s *OpenAIGatewayService) streamLegacyVideo(c *gin.Context, resp *http.Response, idle time.Duration) error {
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (contentType != "video/mp4" && contentType != "application/octet-stream") || resp.Header.Get("Content-Encoding") != "" {
		return videoError(502, "video_output_invalid", "Upstream did not return an unencoded MP4 file")
	}
	expected := int64(-1)
	if length := resp.Header.Get("Content-Length"); length != "" {
		expected, err = strconv.ParseInt(length, 10, 64)
		if err != nil || expected < 1 || expected > videoContentMaxBytes || strconv.FormatInt(expected, 10) != length {
			return videoError(502, "video_output_invalid", "Invalid upstream video length")
		}
	}
	reader := &videoIdleReader{resp.Body, idle}
	header := make([]byte, 12)
	if _, err = io.ReadFull(reader, header); errors.Is(err, context.DeadlineExceeded) {
		return videoError(504, "video_download_timeout", "Video download stalled")
	} else if err != nil || !bytes.Equal(header[4:8], []byte("ftyp")) {
		return videoError(502, "video_output_invalid", "Invalid upstream MP4 signature")
	}
	if expected >= 0 && expected < 12 {
		return videoError(502, "video_output_invalid", "Invalid upstream video length")
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	if expected > 0 {
		c.Header("Content-Length", strconv.FormatInt(expected, 10))
	}
	c.Status(http.StatusOK)
	limit := videoContentMaxBytes
	if expected > 0 {
		limit = expected
	}
	combined := io.MultiReader(bytes.NewReader(header), reader)
	written, err := io.CopyBuffer(c.Writer, io.LimitReader(combined, limit), make([]byte, 32<<10))
	if err != nil {
		return fmt.Errorf("video transfer interrupted: %w", err)
	}
	if written < 12 || (expected >= 0 && written != expected) {
		return fmt.Errorf("video transfer length mismatch")
	}
	// Probe beyond the limit before accepting EOF; a truncated chunked body must abort.
	var extra [1]byte
	n, e := combined.Read(extra[:])
	if n != 0 || e != io.EOF {
		return fmt.Errorf("video transfer exceeded limit or failed")
	}
	return nil
}
