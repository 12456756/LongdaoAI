package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/redis/go-redis/v9"
)

const VideoReferenceFileMaxBytes int64 = 90 << 20

func videoFileMimeType(extension string) string {
	switch extension {
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	default:
		return ""
	}
}

func referenceUploadReadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return videoReferenceTooLarge()
	}
	return videoError(400, "invalid_video_reference_upload", "Expected one non-empty video in the multipart file field.")
}

func videoReferenceTooLarge() error {
	return videoError(413, "video_reference_too_large", "Reference video exceeds 90 MiB or the multipart request exceeds its size limit.")
}

func referenceStorageError(err error) error {
	slog.Warn("video_reference.file_storage_error", "error", err)
	return videoError(503, "video_reference_storage_error", "Reference video storage is temporarily unavailable.")
}

func (s *OpenAIGatewayService) SaveVideoReferenceUpload(ctx context.Context, key *APIKey, reader *multipart.Reader) (*VideoReference, error) {
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" || part.FileName() == "" {
		return nil, referenceUploadReadError(err)
	}
	file, err := os.CreateTemp("", "longdao-video-reference-*")
	if err != nil {
		return nil, referenceStorageError(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()

	hash := sha256.New()
	output := io.MultiWriter(file, hash)
	size, err := io.Copy(output, io.LimitReader(part, VideoReferenceFileMaxBytes+1))
	if size > VideoReferenceFileMaxBytes {
		return nil, videoReferenceTooLarge()
	}
	if err != nil {
		var diskError *os.PathError
		if errors.As(err, &diskError) {
			return nil, referenceStorageError(err)
		}
		return nil, referenceUploadReadError(err)
	}
	if _, err = reader.NextPart(); err != io.EOF || size == 0 {
		return nil, referenceUploadReadError(err)
	}
	extension, err := inspectVideoReferenceFile(file, size)
	if err != nil {
		var diskError *os.PathError
		if errors.As(err, &diskError) {
			return nil, referenceStorageError(err)
		}
		return nil, videoError(415, "unsupported_video_reference_format", "Reference file must be a readable MP4 or MOV with a video track.")
	}

	r, err := s.videoReferences()
	if err != nil {
		return nil, referenceStorageError(err)
	}
	unlock, err := videoDiskLock(ctx, r.cfg.ReferenceDir)
	if err != nil {
		return nil, referenceStorageError(err)
	}
	defer unlock()
	if err = r.checkReferenceCapacity(ctx, size+4096); err != nil {
		return nil, referenceStorageError(err)
	}
	input := videoReferenceInput{path: file.Name(), size: size, sha256: hex.EncodeToString(hash.Sum(nil)), extension: extension, mimeType: videoFileMimeType(extension)}
	ref, err := r.saveReferenceFile(ctx, key, input, r.now().UTC(), true)
	if err != nil {
		return nil, referenceStorageError(err)
	}
	return ref, nil
}

// Inspect the ISO media container without loading or decoding its media payload.
func inspectVideoReferenceFile(file io.ReaderAt, size int64) (string, error) {
	extension := ""
	var hasMedia, hasVideo bool
	err := walkVideoBoxes(file, 0, size, func(kind string, start, end int64) error {
		switch kind {
		case "ftyp":
			var brands [8]byte
			if end-start < int64(len(brands)) || (end-start)%4 != 0 {
				return errInvalidVideoBox
			}
			if _, err := file.ReadAt(brands[:], start); err != nil {
				return err
			}
			extension = ".mp4"
			if string(brands[:4]) == "qt  " {
				extension = ".mov"
			}
		case "mdat":
			hasMedia = hasMedia || start < end
		case "moov":
			return walkVideoBoxes(file, start, end, func(kind string, start, end int64) error {
				if kind != "trak" {
					return nil
				}
				return walkVideoBoxes(file, start, end, func(kind string, start, end int64) error {
					if kind != "mdia" {
						return nil
					}
					return walkVideoBoxes(file, start, end, func(kind string, start, end int64) error {
						if kind != "hdlr" {
							return nil
						}
						var handler [12]byte
						if end-start < int64(len(handler)) {
							return errInvalidVideoBox
						}
						if _, err := file.ReadAt(handler[:], start); err != nil {
							return err
						}
						hasVideo = hasVideo || string(handler[8:]) == "vide"
						return nil
					})
				})
			})
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if extension == "" || !hasMedia || !hasVideo {
		return "", errInvalidVideoBox
	}
	return extension, nil
}

var errInvalidVideoBox = errors.New("invalid video container")

func walkVideoBoxes(file io.ReaderAt, start, end int64, visit func(string, int64, int64) error) error {
	for offset := start; offset < end; {
		var box [16]byte
		header := int64(8)
		if end-offset < header {
			return errInvalidVideoBox
		}
		if _, err := file.ReadAt(box[:8], offset); err != nil {
			return err
		}
		size := uint64(binary.BigEndian.Uint32(box[:4]))
		switch size {
		case 0:
			size = uint64(end - offset)
		case 1:
			header = 16
			if end-offset < header {
				return errInvalidVideoBox
			}
			if _, err := file.ReadAt(box[8:], offset+8); err != nil {
				return err
			}
			size = binary.BigEndian.Uint64(box[8:])
		}
		if size < uint64(header) || size > uint64(end-offset) {
			return errInvalidVideoBox
		}
		next := offset + int64(size)
		if err := visit(string(box[4:8]), offset+header, next); err != nil {
			return err
		}
		offset = next
	}
	return nil
}

func (s *OpenAIGatewayService) resolveVideoReferenceFiles(ctx context.Context, key *APIKey, items []videoReferenceContent) ([]videoReferenceContent, []string, error) {
	resolved := append([]videoReferenceContent(nil), items...)
	var assets []string
	for i, item := range items {
		if item.VideoURL == nil || item.VideoURL.ReferenceID == "" {
			continue
		}
		r, err := s.videoReferences()
		if err != nil {
			return nil, nil, referenceStorageError(err)
		}
		id := item.VideoURL.ReferenceID
		ref, err := r.store.VideoLoadReference(ctx, id)
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, nil, referenceStorageError(err)
		}
		if ref == nil || ref.APIKeyID != key.ID || ref.GroupID != derefGroupID(key.GroupID) {
			return nil, nil, videoError(404, "video_reference_not_found", "Reference video was not found.")
		}
		now := r.now().Unix()
		if now >= ref.ExpiresAt {
			return nil, nil, videoError(410, "video_reference_expired", "Reference video expired. Upload it again.")
		}
		extension := filepath.Ext(ref.RelativePath)
		if ref.AssetID != id || ref.SchemaVersion != 1 || ref.ReadUntil <= ref.ExpiresAt || videoFileMimeType(extension) == "" || videoFileMimeType(extension) != ref.MimeType {
			return nil, nil, referenceStorageError(fmt.Errorf("invalid saved video reference metadata for %s", id))
		}
		expires := min(ref.ReadUntil, now+int64(openAIVideoTaskTTL/time.Second))
		resolved[i].VideoURL = &videoContentURL{URL: r.signedReferenceURL(id, extension, expires)}
		assets = append(assets, id)
	}
	return resolved, assets, nil
}
