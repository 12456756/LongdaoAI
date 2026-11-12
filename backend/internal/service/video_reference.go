package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const VideoUploadMaxBytes int64 = 12 << 20
const videoPNGMaxBytes = 10 << 20

var videoAssetPattern = regexp.MustCompile(`^vref_[a-f0-9]{32}$`)

type videoReferenceRuntime struct {
	cfg               config.VideoLegacyConfig
	store             VideoStateStore
	uploads           chan struct{}
	downloads         chan struct{}
	cancel            context.CancelFunc
	done              chan struct{}
	now               func() time.Time
	uploadFailures    atomic.Int64
	signatureFailures atomic.Int64
	readFailures      atomic.Int64
	cleanupFailures   atomic.Int64
}

func newVideoReferenceRuntime(cfg config.VideoLegacyConfig, store VideoStateStore) (*videoReferenceRuntime, error) {
	if err := cfg.ValidateReference(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(cfg.ReferenceDir) {
		return nil, fmt.Errorf("video reference directory must be absolute")
	}
	if err := os.MkdirAll(cfg.ReferenceDir, 0700); err != nil {
		return nil, fmt.Errorf("create video reference directory")
	}
	info, err := os.Lstat(cfg.ReferenceDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid video reference root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &videoReferenceRuntime{cfg: cfg, store: store, uploads: make(chan struct{}, cfg.UploadConcurrency), downloads: make(chan struct{}, cfg.ReferenceDownloadConcurrency), cancel: cancel, done: make(chan struct{}), now: time.Now}
	go func() {
		defer close(r.done)
		r.cleanup(ctx)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.cleanup(ctx)
			}
		}
	}()
	return r, nil
}

func (s *OpenAIGatewayService) videoReferences() (*videoReferenceRuntime, error) {
	s.videoReferenceOnce.Do(func() {
		store, err := s.videoStore()
		if err != nil {
			s.videoReferenceErr = err
			return
		}
		if s.cfg == nil {
			s.videoReferenceErr = fmt.Errorf("video reference configuration unavailable")
			return
		}
		s.videoReferenceRuntime, s.videoReferenceErr = newVideoReferenceRuntime(s.cfg.VideoLegacy, store)
	})
	return s.videoReferenceRuntime, s.videoReferenceErr
}

func (s *OpenAIGatewayService) AcquireVideoUpload(ctx context.Context) (func(), error) {
	// Bound memory before parsing; the runtime storage may remain disabled for native accounts.
	max := 2
	if s.cfg != nil && s.cfg.VideoLegacy.UploadConcurrency > 0 {
		max = s.cfg.VideoLegacy.UploadConcurrency
	}
	s.videoUploadOnce.Do(func() { s.videoUploads = make(chan struct{}, max) })
	select {
	case s.videoUploads <- struct{}{}:
		return func() { <-s.videoUploads }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, fmt.Errorf("video upload concurrency limit reached")
	}
}

type videoReferenceManifest struct {
	SchemaVersion int    `json:"schema_version"`
	AssetID       string `json:"asset_id"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at"`
	SHA256        string `json:"sha256"`
	ByteLength    int64  `json:"byte_length"`
}

func (r *videoReferenceRuntime) unsigned(asset string, expires int64, kid string) string {
	query := url.Values{"expires": {strconv.FormatInt(expires, 10)}, "kid": {kid}}
	return strings.TrimRight(r.cfg.PublicBaseURL, "/") + "/media/video-inputs/" + asset + ".png?" + query.Encode()
}

func (r *videoReferenceRuntime) sign(asset string, expires int64, kid string) string {
	key, _ := base64.StdEncoding.DecodeString(r.cfg.SigningKeys[kid])
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(r.unsigned(asset, expires, kid)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (r *videoReferenceRuntime) save(ctx context.Context, key *APIKey, data []byte) (reference *VideoReference, saveErr error) {
	defer func() {
		if saveErr != nil {
			r.uploadFailures.Add(1)
			slog.Warn("video_reference.upload_failed", "api_key_id", key.ID)
		}
	}()
	if len(data) == 0 || len(data) > videoPNGMaxBytes {
		return nil, videoError(400, "invalid_request_error", "input_reference must be a PNG no larger than 10 MiB")
	}
	select {
	case r.uploads <- struct{}{}:
		defer func() { <-r.uploads }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	dimensions, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || dimensions.Width < 1 || dimensions.Height < 1 || dimensions.Width > 4096 || dimensions.Height > 4096 {
		return nil, videoError(400, "invalid_request_error", "input_reference must be a valid PNG at most 4096x4096")
	}
	if _, err = png.Decode(bytes.NewReader(data)); err != nil {
		return nil, videoError(400, "invalid_request_error", "input_reference PNG decoding failed")
	}
	unlock, err := videoDiskLock(ctx, r.cfg.ReferenceDir)
	if err != nil {
		return nil, fmt.Errorf("video reference storage busy")
	}
	defer unlock()
	var used int64
	err = filepath.WalkDir(r.cfg.ReferenceDir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in reference storage")
		}
		if !d.IsDir() {
			i, e := d.Info()
			if e != nil {
				return e
			}
			used += i.Size()
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("video reference capacity scan failed")
	}
	required := int64(len(data)) + 4096
	free, err := videoDiskFree(r.cfg.ReferenceDir)
	if err != nil || used+required > r.cfg.MaxStorageBytes || free < r.cfg.MinFreeBytes+required {
		slog.Warn("video_reference.capacity_exceeded", "bytes", used)
		return nil, fmt.Errorf("video reference storage capacity exceeded")
	}
	now := r.now().UTC()
	asset := "vref_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	parent := filepath.Join(r.cfg.ReferenceDir, now.Format("2006/01/02"))
	if err = os.MkdirAll(parent, 0700); err != nil {
		return nil, fmt.Errorf("create video reference date directory")
	}
	staging := filepath.Join(parent, ".upload_"+asset)
	if err = os.Mkdir(staging, 0700); err != nil {
		return nil, fmt.Errorf("create video reference staging")
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	digest := sha256.Sum256(data)
	ref := &VideoReference{SchemaVersion: 1, AssetID: asset, APIKeyID: key.ID, GroupID: derefGroupID(key.GroupID), RelativePath: filepath.ToSlash(filepath.Join(now.Format("2006/01/02"), asset, "input.png")), MimeType: "image/png", ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), Width: dimensions.Width, Height: dimensions.Height, CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 604800, SigningKeyID: r.cfg.SigningKeyID}
	ref.SignedURL = r.unsigned(asset, ref.ExpiresAt, ref.SigningKeyID) + "&signature=" + r.sign(asset, ref.ExpiresAt, ref.SigningKeyID)
	if err = writeVideoFile(filepath.Join(staging, "input.png"), data); err != nil {
		return nil, err
	}
	manifest, _ := json.Marshal(videoReferenceManifest{1, asset, ref.CreatedAt, ref.ExpiresAt, ref.SHA256, ref.ByteLength})
	if err = writeVideoFile(filepath.Join(staging, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	final := filepath.Join(parent, asset)
	if _, err = os.Lstat(final); !os.IsNotExist(err) {
		return nil, fmt.Errorf("video reference collision")
	}
	if err = os.Rename(staging, final); err != nil {
		return nil, fmt.Errorf("publish video reference")
	}
	published = true
	if err = r.store.VideoSaveReference(ctx, ref); err != nil {
		_ = os.RemoveAll(final)
		return nil, fmt.Errorf("register video reference")
	}
	slog.Info("video_reference.saved", "asset_id", asset, "bytes", ref.ByteLength, "expires_at", ref.ExpiresAt)
	return ref, nil
}

func writeVideoFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create video reference file")
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("write video reference file")
	}
	return nil
}

func (s *OpenAIGatewayService) ServeVideoReference(w http.ResponseWriter, req *http.Request, filename string) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	r, err := s.videoReferences()
	if err != nil {
		http.Error(w, "video reference service unavailable", 503)
		return
	}
	asset := strings.TrimSuffix(filename, ".png")
	query, parseErr := url.ParseQuery(req.URL.RawQuery)
	valid := parseErr == nil && strings.HasSuffix(filename, ".png") && videoAssetPattern.MatchString(asset) && len(query) == 3
	for _, name := range []string{"expires", "kid", "signature"} {
		valid = valid && len(query[name]) == 1
	}
	expires, err := strconv.ParseInt(query.Get("expires"), 10, 64)
	kid := query.Get("kid")
	sig, decodeErr := hex.DecodeString(query.Get("signature"))
	_, known := r.cfg.SigningKeys[kid]
	if !valid || err != nil || strconv.FormatInt(expires, 10) != query.Get("expires") || !known || decodeErr != nil || len(sig) != 32 {
		r.signatureFailures.Add(1)
		slog.Warn("video_reference.signature_rejected")
		http.Error(w, "invalid signature", 403)
		return
	}
	expected, _ := hex.DecodeString(r.sign(asset, expires, kid))
	if !hmac.Equal(expected, sig) {
		r.signatureFailures.Add(1)
		slog.Warn("video_reference.signature_rejected")
		http.Error(w, "invalid signature", 403)
		return
	}
	if r.now().Unix() >= expires {
		http.Error(w, "reference expired", 410)
		return
	}
	readError := func(status int, message string) {
		r.readFailures.Add(1)
		slog.Warn("video_reference.read_failed", "asset_id", asset, "status", status)
		http.Error(w, message, status)
	}
	ref, err := r.store.VideoLoadReference(req.Context(), asset)
	if errors.Is(err, redis.Nil) {
		readError(404, "reference not found")
		return
	}
	if err != nil {
		readError(503, "reference storage unavailable")
		return
	}
	if ref.AssetID != asset || ref.ExpiresAt != expires || ref.SigningKeyID != kid || ref.SchemaVersion != 1 || ref.MimeType != "image/png" || ref.ByteLength < 1 || ref.ByteLength > videoPNGMaxBytes {
		readError(403, "invalid reference")
		return
	}
	select {
	case r.downloads <- struct{}{}:
		defer func() { <-r.downloads }()
	default:
		readError(429, "reference download busy")
		return
	}
	root, err := os.OpenRoot(r.cfg.ReferenceDir)
	if err != nil {
		readError(503, "reference storage unavailable")
		return
	}
	defer root.Close()
	relative := filepath.FromSlash(ref.RelativePath)
	expectedPath := filepath.Join(time.Unix(ref.CreatedAt, 0).UTC().Format("2006/01/02"), asset, "input.png")
	if relative != expectedPath {
		readError(403, "invalid reference")
		return
	}
	// Reject symlinks in every component, even links that stay inside the root.
	parts := strings.Split(filepath.ToSlash(relative), "/")
	partPath := ""
	for _, part := range parts {
		partPath = filepath.Join(partPath, part)
		i, e := root.Lstat(partPath)
		if e != nil {
			if os.IsNotExist(e) {
				readError(404, "reference not found")
			} else {
				readError(503, "reference read failed")
			}
			return
		}
		if i.Mode()&os.ModeSymlink != 0 {
			readError(403, "invalid reference")
			return
		}
	}
	f, err := root.Open(relative)
	if err != nil {
		readError(503, "reference read failed")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != ref.ByteLength {
		readError(503, "reference file invalid")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var reader io.ReadSeeker = f
	if r.cfg.ReferenceBytesPerSecond > 0 {
		reader = &videoThrottledFile{ReadSeeker: f, ctx: req.Context(), rate: r.cfg.ReferenceBytesPerSecond, start: time.Now()}
	}
	http.ServeContent(w, req, "input.png", time.Unix(ref.CreatedAt, 0), reader)
}

type videoThrottledFile struct {
	io.ReadSeeker
	ctx   context.Context
	rate  int
	start time.Time
	bytes int64
}

func (r *videoThrottledFile) Read(p []byte) (int, error) {
	if len(p) > 32<<10 {
		p = p[:32<<10]
	}
	n, err := r.ReadSeeker.Read(p)
	r.bytes += int64(n)
	wait := time.Duration(float64(r.bytes)/float64(r.rate)*float64(time.Second)) - time.Since(r.start)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.ctx.Done():
			return n, r.ctx.Err()
		}
	}
	return n, err
}

func (r *videoReferenceRuntime) cleanup(ctx context.Context) {
	unlock, err := videoDiskLock(ctx, r.cfg.ReferenceDir)
	if err != nil {
		return
	}
	defer unlock()
	now := r.now().Unix()
	var directoryBytes, directoryFiles, expiredBacklog int64
	defer func() {
		slog.Info("video_reference.metrics", "directory_bytes", directoryBytes, "directory_files", directoryFiles,
			"expired_backlog", expiredBacklog, "upload_failures", r.uploadFailures.Load(),
			"signature_failures", r.signatureFailures.Load(), "read_failures", r.readFailures.Load(),
			"cleanup_failures", r.cleanupFailures.Load())
	}()
	err = filepath.WalkDir(r.cfg.ReferenceDir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".upload_vref_") {
			info, e := d.Info()
			if e == nil && now-info.ModTime().Unix() > 3600 {
				if e = os.RemoveAll(path); e != nil {
					r.cleanupFailures.Add(1)
					slog.Warn("video_reference.staging_cleanup_failed")
				}
			}
			return filepath.SkipDir
		}
		if !d.IsDir() {
			info, e := d.Info()
			if e != nil {
				return e
			}
			directoryBytes += info.Size()
			directoryFiles++
		}
		if d.Name() != "manifest.json" || d.IsDir() {
			return nil
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		data, e := io.ReadAll(io.LimitReader(f, 4097))
		f.Close()
		var m videoReferenceManifest
		if e != nil || len(data) > 4096 || json.Unmarshal(data, &m) != nil || m.SchemaVersion != 1 || !videoAssetPattern.MatchString(m.AssetID) || filepath.Base(filepath.Dir(path)) != m.AssetID || m.ExpiresAt != m.CreatedAt+604800 {
			r.cleanupFailures.Add(1)
			slog.Warn("video_reference.invalid_manifest")
			return nil
		}
		if now < m.ExpiresAt {
			return nil
		}
		dir := filepath.Dir(path)
		expiredBacklog++
		imagePath := filepath.Join(dir, "input.png")
		imageInfo, _ := os.Lstat(imagePath)
		if e = os.Remove(imagePath); e != nil && !os.IsNotExist(e) {
			r.cleanupFailures.Add(1)
			slog.Warn("video_reference.cleanup_failed", "asset_id", m.AssetID)
			return nil
		}
		if imageInfo != nil && imageInfo.Mode().IsRegular() {
			directoryBytes -= imageInfo.Size()
			directoryFiles--
		}
		if e = os.Remove(path); e != nil {
			return e
		}
		directoryBytes -= int64(len(data))
		directoryFiles--
		expiredBacklog--
		_ = os.Remove(dir)
		for parent := filepath.Dir(dir); parent != r.cfg.ReferenceDir; parent = filepath.Dir(parent) {
			if e = os.Remove(parent); e != nil {
				break
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		r.cleanupFailures.Add(1)
		slog.Warn("video_reference.cleanup_scan_failed")
	}
}
