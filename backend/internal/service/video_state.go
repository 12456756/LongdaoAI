package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const VideoProtocolOpenAI = "openai_videos"
const VideoProtocolLegacy = "naoshengyun_legacy"

func (a *Account) VideoProtocol() string {
	if v := a.GetCredential("video_protocol"); v != "" {
		return v
	}
	return VideoProtocolOpenAI
}

func ValidateVideoProtocolCredentials(credentials map[string]any) error {
	if v, exists := credentials["video_protocol"]; exists && v != VideoProtocolOpenAI && v != VideoProtocolLegacy {
		return infraerrors.BadRequest("INVALID_VIDEO_PROTOCOL", "video_protocol must be openai_videos or naoshengyun_legacy")
	}
	return nil
}

type VideoTaskMetadata struct {
	SchemaVersion    int    `json:"schema_version"`
	TaskID           string `json:"task_id"`
	APIKeyID         int64  `json:"api_key_id"`
	GroupID          int64  `json:"group_id"`
	AccountID        int64  `json:"account_id"`
	Protocol         string `json:"protocol"`
	RequestedModel   string `json:"requested_model"`
	UpstreamModel    string `json:"upstream_model"`
	ReferenceAssetID string `json:"reference_asset_id,omitempty"`
	BillingRequestID string `json:"billing_request_id"`
	LastStatus       string `json:"last_status"`
	BoundAt          int64  `json:"bound_at"`
	ExpiresAt        int64  `json:"expires_at"`
}

type VideoReference struct {
	SchemaVersion int    `json:"schema_version"`
	AssetID       string `json:"asset_id"`
	APIKeyID      int64  `json:"api_key_id"`
	GroupID       int64  `json:"group_id"`
	RelativePath  string `json:"relative_path"`
	MimeType      string `json:"mime_type"`
	ByteLength    int64  `json:"byte_length"`
	SHA256        string `json:"sha256"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at"`
	SigningKeyID  string `json:"signing_key_id"`
	SignedURL     string `json:"signed_url"`
	TaskID        string `json:"task_id"`
}

// VideoStateStore is implemented by the existing Redis gateway repository.
type VideoStateStore interface {
	VideoLoad(context.Context, string) ([]byte, error)
	VideoSave(context.Context, string, []byte, time.Time, bool) (bool, error)
	VideoReplace(context.Context, string, []byte) (bool, error)
	VideoBind(context.Context, *VideoTaskMetadata) error
	VideoStatus(context.Context, string, string) (string, error)
	VideoSaveReference(context.Context, *VideoReference) error
	VideoLoadReference(context.Context, string) (*VideoReference, error)
	VideoReferenceTask(context.Context, string, string) error
}

func VideoTaskMetadataKey(group, key int64, task string) string {
	return fmt.Sprintf("video:task-meta:%d:%d:%s", group, key, DeriveSessionHashFromSeed(task))
}

func (s *OpenAIGatewayService) videoStore() (VideoStateStore, error) {
	store, ok := s.cache.(VideoStateStore)
	if !ok {
		return nil, fmt.Errorf("video task metadata storage is unavailable")
	}
	return store, nil
}

func (s *OpenAIGatewayService) videoTaskMetadata(ctx context.Context, key *APIKey, task string) (*VideoTaskMetadata, error) {
	store, err := s.videoStore()
	if err != nil {
		return nil, err
	}
	data, err := store.VideoLoad(ctx, VideoTaskMetadataKey(derefGroupID(key.GroupID), key.ID, task))
	if err != nil {
		return nil, err
	}
	var m VideoTaskMetadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid video task metadata")
	}
	if m.SchemaVersion != 1 || m.TaskID != task || m.APIKeyID != key.ID || m.GroupID != derefGroupID(key.GroupID) || m.ExpiresAt <= time.Now().Unix() {
		return nil, fmt.Errorf("video task metadata mismatch")
	}
	return &m, nil
}
