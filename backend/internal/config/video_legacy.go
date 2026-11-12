package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/viper"
)

type VideoLegacyConfig struct {
	ReferenceDir                 string            `mapstructure:"reference_dir"`
	PublicBaseURL                string            `mapstructure:"public_base_url"`
	SigningKeyID                 string            `mapstructure:"signing_key_id"`
	SigningKeys                  map[string]string `mapstructure:"signing_keys"`
	MaxStorageBytes              int64             `mapstructure:"max_storage_bytes"`
	MinFreeBytes                 int64             `mapstructure:"min_free_bytes"`
	UploadConcurrency            int               `mapstructure:"upload_concurrency"`
	ReferenceDownloadConcurrency int               `mapstructure:"reference_download_concurrency"`
	ReferenceBytesPerSecond      int               `mapstructure:"reference_bytes_per_second"`
	DownloadTimeoutSeconds       int               `mapstructure:"download_timeout_seconds"`
	DownloadIdleSeconds          int               `mapstructure:"download_idle_seconds"`
}

func setVideoLegacyDefaults() {
	viper.SetDefault("video_legacy.reference_dir", "/app/media/video-inputs")
	viper.SetDefault("video_legacy.upload_concurrency", 2)
	viper.SetDefault("video_legacy.reference_download_concurrency", 4)
	viper.SetDefault("video_legacy.download_timeout_seconds", 150)
	viper.SetDefault("video_legacy.download_idle_seconds", 20)
}

// Validate is also used at request time so incomplete configuration cannot dispatch a task.
func (c VideoLegacyConfig) ValidateReference() error {
	u, err := url.Parse(c.PublicBaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return fmt.Errorf("video_legacy.public_base_url must be an HTTPS origin")
	}
	if c.ReferenceDir == "" || c.SigningKeyID == "" || c.UploadConcurrency < 1 || c.ReferenceDownloadConcurrency < 1 || c.MaxStorageBytes < 1 || c.MinFreeBytes < 0 {
		return fmt.Errorf("video reference storage, capacity and concurrency must be configured")
	}
	if _, ok := c.SigningKeys[c.SigningKeyID]; !ok {
		return fmt.Errorf("video reference signing key is missing")
	}
	for id, encoded := range c.SigningKeys {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if id == "" || strings.ContainsAny(id, "&/?= ") || err != nil || len(key) < 32 {
			return fmt.Errorf("video reference signing keys must contain at least 32 random bytes encoded as base64")
		}
	}
	return nil
}
