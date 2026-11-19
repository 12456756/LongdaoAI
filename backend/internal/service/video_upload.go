package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	imagecodec "image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const videoMultipartFieldsMaxBytes = 2 << 20
const videoReferenceMaxCount = 9
const videoReferencesMaxBytes int64 = 90 << 20
const nativeVideoUploadMaxBytes int64 = 12 << 20

type videoReferenceInput struct {
	path          string
	data          []byte
	size          int64
	sha256        string
	mimeType      string
	extension     string
	width, height int
}

func videoImageFormat(format string) (mimeType, extension string, ok bool) {
	extension = "." + format
	if format == "jpeg" {
		extension = ".jpg"
	}
	mimeType, ok = videoImageMimeType(extension)
	return mimeType, extension, ok
}

func (image videoReferenceInput) open() (io.ReadCloser, error) {
	if image.path != "" {
		return os.Open(image.path)
	}
	return io.NopCloser(bytes.NewReader(image.data)), nil
}

func (image *videoReferenceInput) validate(index int) error {
	if image.path != "" {
		info, err := os.Stat(image.path)
		if err != nil {
			return videoError(503, "video_upload_storage_error", fmt.Sprintf("Stat input_reference[%d]: %s", index, err))
		}
		image.size = info.Size()
	} else {
		image.size = int64(len(image.data))
	}
	if image.size == 0 || image.size > videoReferenceImageMaxBytes {
		status := 400
		if image.size > videoReferenceImageMaxBytes {
			status = 413
		}
		return videoError(status, "invalid_request_error", fmt.Sprintf("input_reference[%d] size %d bytes; allowed 1..%d bytes", index, image.size, videoReferenceImageMaxBytes))
	}
	file, err := image.open()
	if err != nil {
		return videoError(503, "video_upload_storage_error", fmt.Sprintf("Open input_reference[%d]: %s", index, err))
	}
	config, format, err := imagecodec.DecodeConfig(file)
	file.Close()
	if err != nil {
		return videoError(400, "invalid_request_error", fmt.Sprintf("input_reference[%d] is not a valid image: %s", index, err))
	}
	mimeType, extension, ok := videoImageFormat(format)
	if !ok {
		return videoError(400, "invalid_request_error", fmt.Sprintf("input_reference[%d] has unsupported image format %q", index, format))
	}
	if config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 || int64(config.Width)*int64(config.Height) > 16777216 {
		return videoError(400, "invalid_request_error", fmt.Sprintf("input_reference[%d] dimensions %dx%d; allowed width/height 1..4096 and at most 16777216 pixels", index, config.Width, config.Height))
	}
	file, err = image.open()
	if err != nil {
		return videoError(503, "video_upload_storage_error", fmt.Sprintf("Open input_reference[%d] for decoding: %s", index, err))
	}
	_, decodedFormat, err := imagecodec.Decode(file)
	file.Close()
	if err != nil || decodedFormat != format {
		if err == nil {
			err = fmt.Errorf("image format changed between validation passes")
		}
		return videoError(400, "invalid_request_error", fmt.Sprintf("input_reference[%d] image decoding failed: %s", index, err))
	}
	file, err = image.open()
	if err != nil {
		return videoError(503, "video_upload_storage_error", fmt.Sprintf("Open input_reference[%d] for hashing: %s", index, err))
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	file.Close()
	if err != nil {
		return videoError(503, "video_upload_storage_error", fmt.Sprintf("Hash input_reference[%d]: %s", index, err))
	}
	image.sha256 = hex.EncodeToString(hash.Sum(nil))
	image.mimeType, image.extension = mimeType, extension
	image.width, image.height = config.Width, config.Height
	return nil
}

func (r *OpenAIVideoRequest) ValidateVideoReferences(protocol, operation string) error {
	if protocol != VideoProtocolLegacy || r.legacyValidated {
		return nil
	}
	if r.otherFiles != 0 || len(r.moderation.InputImageURLs) != 0 {
		return videoError(422, "unsupported_parameter", "Only input_reference image uploads are supported")
	}
	count := len(r.moderation.Uploads)
	if r.bodyPath != "" {
		count = r.spooledReferenceCount
	}
	switch operation {
	case "text-to-video":
		if count != 0 {
			return videoError(400, "invalid_request_error", fmt.Sprintf("Text-to-video requires 0 reference images; received %d", count))
		}
	case "image-to-video":
		if count < 1 || count > videoReferenceMaxCount {
			return videoError(400, "invalid_request_error", fmt.Sprintf("input_reference count %d; allowed 1..%d", count, videoReferenceMaxCount))
		}
	default:
		return videoError(400, "invalid_request_error", "X-Video-Operation must be text-to-video or image-to-video")
	}
	var images []videoReferenceInput
	var total int64
	for i, upload := range r.moderation.Uploads {
		image := videoReferenceInput{data: upload.Data}
		if i < len(r.uploadPaths) {
			image.path = r.uploadPaths[i]
		}
		if err := image.validate(i + 1); err != nil {
			return err
		}
		total += image.size
		if total > videoReferencesMaxBytes {
			return videoError(413, "invalid_request_error", fmt.Sprintf("input_reference total %d bytes exceeds limit %d bytes", total, videoReferencesMaxBytes))
		}
		images = append(images, image)
	}
	r.legacyImages, r.legacyValidated = images, true
	return nil
}

// ParseOpenAIVideoUpload spools the wire body and uploads without retaining image bytes.
// The wire body is retained for accounts using the native video protocol.
func ParseOpenAIVideoUpload(body io.Reader, contentType string) (request *OpenAIVideoRequest, err error) {
	mediaType, params, e := mime.ParseMediaType(contentType)
	if e != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, videoError(400, "invalid_request_error", "multipart boundary is required")
	}
	r := &OpenAIVideoRequest{ContentType: contentType, Multipart: true}
	r.tempDir, err = os.MkdirTemp("", "longdao-video-upload-")
	if err != nil {
		return nil, videoError(503, "video_upload_storage_error", "Create video upload staging directory: "+err.Error())
	}
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	r.bodyPath = filepath.Join(r.tempDir, "request.multipart")
	raw, err := os.OpenFile(r.bodyPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, videoError(503, "video_upload_storage_error", "Create video upload spool: "+err.Error())
	}
	defer raw.Close()
	hash := sha256.New()
	// Also enforce the limit when called outside the HTTP handler, including chunked bodies.
	limited := http.MaxBytesReader(nil, io.NopCloser(body), VideoUploadMaxBytes)
	wire := io.TeeReader(limited, io.MultiWriter(raw, hash))
	reader := multipart.NewReader(wire, params["boundary"])
	fields := make(map[string]string)
	var fieldBytes int64
	r.moderation.N = 1
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, videoUploadReadError(e)
		}
		name := part.FormName()
		if part.FileName() == "" {
			value, e := io.ReadAll(io.LimitReader(part, videoMultipartFieldsMaxBytes-fieldBytes+1))
			if e != nil {
				return nil, videoUploadReadError(e)
			}
			fieldBytes += int64(len(value))
			if fieldBytes > videoMultipartFieldsMaxBytes {
				return nil, videoError(413, "invalid_request_error", fmt.Sprintf("Multipart fields exceed limit %d bytes", videoMultipartFieldsMaxBytes))
			}
			if _, exists := fields[name]; exists {
				return nil, videoError(400, "invalid_request_error", "duplicate multipart field: "+name)
			}
			fields[name] = string(value)
		} else {
			if name != "input_reference" {
				r.otherFiles++
			}
			if name == "input_reference" {
				r.spooledReferenceCount++
			}
			if name == "input_reference" && r.spooledReferenceCount <= videoReferenceMaxCount {
				path := filepath.Join(r.tempDir, fmt.Sprintf("image-%d", len(r.uploadPaths)))
				file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if e != nil {
					return nil, videoError(503, "video_upload_storage_error", "Create uploaded image spool: "+e.Error())
				}
				_, e = io.Copy(file, part)
				closeErr := file.Close()
				if e != nil {
					return nil, videoUploadReadError(e)
				}
				if closeErr != nil {
					return nil, videoError(503, "video_upload_storage_error", "Close uploaded image spool: "+closeErr.Error())
				}
				r.uploadPaths = append(r.uploadPaths, path)
				r.moderation.Uploads = append(r.moderation.Uploads, OpenAIImagesUpload{FieldName: name, FileName: part.FileName(), ContentType: part.Header.Get("Content-Type")})
			}
		}
		if e = part.Close(); e != nil {
			return nil, videoUploadReadError(e)
		}
	}
	// Consume the epilogue as well so it cannot bypass size checking or the wire hash.
	if _, e = io.Copy(io.Discard, wire); e != nil {
		return nil, videoUploadReadError(e)
	}
	r.bodyHash = hex.EncodeToString(hash.Sum(nil))
	r.wireBytes, e = raw.Seek(0, io.SeekCurrent)
	if e != nil {
		return nil, videoUploadReadError(e)
	}
	for _, name := range []string{"image", "image_url"} {
		if value := strings.TrimSpace(fields[name]); value != "" {
			r.moderation.InputImageURLs = append(r.moderation.InputImageURLs, value)
		}
	}
	r.moderation.MaskImageURL = firstNonEmptyString(fields["mask"], fields["mask_image_url"])
	return finishOpenAIVideoRequest(r, fields)
}

func videoUploadReadError(err error) error {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		return videoError(413, "invalid_request_error", fmt.Sprintf("Video multipart request exceeds limit %d bytes", max.Limit))
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return videoError(503, "video_upload_storage_error", "Write video upload spool: "+err.Error())
	}
	return videoError(400, "invalid_request_error", "Read video multipart upload: "+err.Error())
}

func VideoRequestErrorDetails(err error) (int, string, string) {
	var protocolErr *videoProtocolError
	if errors.As(err, &protocolErr) {
		return protocolErr.status, protocolErr.code, protocolErr.message
	}
	return 400, "invalid_request_error", err.Error()
}

func (r *OpenAIVideoRequest) Close() {
	if r.tempDir != "" {
		_ = os.RemoveAll(r.tempDir)
	}
}

func (r *OpenAIVideoRequest) PayloadHash() string {
	if r.bodyHash != "" {
		return r.bodyHash
	}
	return HashUsageRequestPayload(r.Body)
}

// VisitModerationBodies keeps at most one uploaded image's base64 in each check.
// This also avoids the shared moderation policy sampling one image from a batch.
func (r *OpenAIVideoRequest) VisitModerationBodies(visit func([]byte) bool) error {
	if !r.Multipart {
		visit(r.ModerationBody())
		return nil
	}
	info := r.moderation
	info.Uploads = nil
	for i, upload := range r.moderation.Uploads {
		if i < len(r.uploadPaths) {
			data, err := os.ReadFile(r.uploadPaths[i])
			if err != nil {
				return videoError(503, "video_upload_storage_error", fmt.Sprintf("Read input_reference[%d] for moderation: %s", i+1, err))
			}
			upload.Data = data
		}
		// Use the format verified by ValidateVideoReferences, not the client header.
		if i < len(r.legacyImages) {
			upload.ContentType = r.legacyImages[i].mimeType
		}
		info.Uploads = []OpenAIImagesUpload{upload}
		if !visit(info.ModerationBody()) {
			return nil
		}
	}
	if len(r.moderation.Uploads) == 0 {
		visit(info.ModerationBody())
	}
	return nil
}

func (r *OpenAIVideoRequest) PrepareNativeVideoUpload() error {
	if r.bodyPath == "" {
		return nil
	}
	if r.wireBytes > nativeVideoUploadMaxBytes {
		return videoError(413, "invalid_request_error", fmt.Sprintf("Native video multipart size %d bytes exceeds limit %d bytes", r.wireBytes, nativeVideoUploadMaxBytes))
	}
	body, err := os.ReadFile(r.bodyPath)
	if err != nil {
		return videoError(503, "video_upload_storage_error", "Read native video upload spool: "+err.Error())
	}
	parsed, err := ParseOpenAIVideoRequest(body, r.ContentType)
	if err != nil {
		return err
	}
	r.Body, r.moderation = parsed.Body, parsed.moderation
	return nil
}
