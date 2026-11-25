package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var asyncImageAllowedFields = map[string]bool{"model": true, "prompt": true, "n": true, "size": true, "quality": true, "response_format": true, "output_format": true, "output_compression": true, "background": true, "moderation": true, "stream": true, "mask": true, "image_urls": true, "image_url": true, "images": true, "image": true, "image[]": true}

// Used only to choose protocol validation. Dispatch always uses the strict parser.
func AsyncImageModelHint(c *gin.Context, body []byte) string {
	mediaType, params, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	model := ""
	if mediaType == "multipart/form-data" && params["boundary"] != "" {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			if part.FormName() == "model" && part.FileName() == "" {
				data, _ := io.ReadAll(io.LimitReader(part, 201))
				model = strings.TrimSpace(string(data))
				part.Close()
				break
			}
			part.Close()
		}
	} else {
		model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	if model == "" {
		model = "gpt-image-2"
	}
	return model
}

func (s *OpenAIGatewayService) ParseAsyncImageRequest(c *gin.Context, body []byte) (*OpenAIImagesRequest, error) {
	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		return nil, imageLocalError(400, "invalid_content_type", "Expected application/json or multipart/form-data: "+err.Error())
	}
	if mediaType == "multipart/form-data" {
		if len(body) > asyncImageMultipartMaxBytes {
			return nil, imageLocalError(413, "image_request_too_large", fmt.Sprintf("Multipart image request exceeds %d bytes", asyncImageMultipartMaxBytes))
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		files := 0
		seen := map[string]bool{}
		for {
			part, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, imageLocalError(400, "invalid_multipart", e.Error())
			}
			name := part.FormName()
			if !asyncImageAllowedFields[name] && !strings.HasPrefix(name, "image[") {
				part.Close()
				return nil, imageLocalError(422, "unsupported_parameter", "Unsupported asynchronous image field: "+name)
			}
			if name == "mask" || name == "stream" {
				part.Close()
				return nil, imageLocalError(422, "unsupported_parameter", "Asynchronous images do not accept parameter: "+name)
			}
			limit := int64(64 << 10)
			if part.FileName() != "" {
				if name != "image" && !strings.HasPrefix(name, "image[") {
					part.Close()
					return nil, imageLocalError(422, "unsupported_parameter", "Only image/image[] reference uploads are supported")
				}
				files++
				if files > 4 {
					part.Close()
					return nil, imageLocalError(422, "too_many_reference_images", "At most 4 reference images are supported")
				}
				limit = asyncImageReferenceMaxBytes
			} else {
				if name == "image" || name == "image[]" || name == "image_urls" || name == "image_url" || name == "images" || strings.HasPrefix(name, "image[") {
					part.Close()
					return nil, imageLocalError(422, "unsupported_parameter", "Multipart reference images must be image/image[] file parts")
				}
				if seen[name] {
					part.Close()
					return nil, imageLocalError(400, "duplicate_parameter", "Duplicate image parameter: "+name)
				}
				seen[name] = true
			}
			data, e := io.ReadAll(io.LimitReader(part, limit+1))
			part.Close()
			if e != nil {
				return nil, imageLocalError(400, "invalid_multipart", e.Error())
			}
			if int64(len(data)) > limit {
				return nil, imageLocalError(413, "image_field_too_large", fmt.Sprintf("Image field %s exceeds %d bytes", name, limit))
			}
		}
		parsed, err := s.ParseOpenAIImagesRequest(c, body)
		if err != nil {
			return nil, imageLocalError(400, "invalid_image_request", err.Error())
		}
		return parsed, nil
	}
	if mediaType != "application/json" {
		return nil, imageLocalError(415, "invalid_content_type", "Expected application/json or multipart/form-data")
	}
	if len(body) > asyncImageJSONMaxBytes {
		return nil, imageLocalError(413, "image_request_too_large", fmt.Sprintf("JSON image request exceeds %d bytes", asyncImageJSONMaxBytes))
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, imageLocalError(400, "invalid_json", "Expected one JSON object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, imageLocalError(400, "invalid_json", err.Error())
		}
		name, ok := token.(string)
		if !ok {
			return nil, imageLocalError(400, "invalid_json", "Expected a field name")
		}
		if _, exists := fields[name]; exists {
			return nil, imageLocalError(400, "duplicate_parameter", "Duplicate image parameter: "+name)
		}
		if !asyncImageAllowedFields[name] {
			return nil, imageLocalError(422, "unsupported_parameter", "Unsupported asynchronous image field: "+name)
		}
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil {
			return nil, imageLocalError(400, "invalid_json", err.Error())
		}
		fields[name] = raw
	}
	if _, err = decoder.Token(); err != nil {
		return nil, imageLocalError(400, "invalid_json", err.Error())
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, imageLocalError(400, "invalid_json", "Expected one JSON object")
	}
	for name, raw := range fields {
		if name == "image[]" {
			return nil, imageLocalError(422, "unsupported_parameter", "image[] is a multipart file field; use images or image_urls in JSON")
		}
		if name == "n" || name == "output_compression" {
			var n int
			if json.Unmarshal(raw, &n) != nil || string(raw) == "null" {
				return nil, imageLocalError(422, "invalid_parameter", name+" must be an integer")
			}
			if name == "n" && n != 1 {
				return nil, imageLocalError(422, "unsupported_parameter", "Asynchronous image generation requires n=1")
			}
		}
		if name == "mask" || name == "stream" {
			return nil, imageLocalError(422, "unsupported_parameter", "Asynchronous images do not accept parameter: "+name)
		}
		if name != "n" && name != "output_compression" && name != "images" && name != "image" && name != "image_urls" {
			var str string
			if json.Unmarshal(raw, &str) != nil || string(raw) == "null" {
				return nil, imageLocalError(422, "invalid_parameter", name+" must be a string")
			}
		}
	}
	// Normalize the provider's reference aliases into the existing edit parser.
	var references []string
	aliases := 0
	for _, name := range []string{"image_urls", "image_url", "image"} {
		if raw, ok := fields[name]; ok {
			aliases++
			var value string
			if json.Unmarshal(raw, &value) == nil {
				references = []string{value}
			} else if json.Unmarshal(raw, &references) != nil {
				return nil, imageLocalError(422, "invalid_reference_image", name+" must contain image strings")
			}
			if len(references) == 0 {
				return nil, imageLocalError(422, "missing_reference_image", "Reference image array must not be empty")
			}
			delete(fields, name)
		}
	}
	if _, ok := fields["images"]; ok {
		aliases++
		var items []map[string]json.RawMessage
		if json.Unmarshal(fields["images"], &items) != nil || len(items) < 1 || len(items) > 4 {
			return nil, imageLocalError(422, "invalid_reference_image", "images must contain 1..4 objects with an image_url string")
		}
		for _, item := range items {
			var reference string
			if len(item) != 1 || json.Unmarshal(item["image_url"], &reference) != nil || strings.TrimSpace(reference) == "" {
				return nil, imageLocalError(422, "invalid_reference_image", "Every images item must contain only a nonempty image_url string")
			}
		}
	}
	if aliases > 1 {
		return nil, imageLocalError(422, "invalid_reference_image", "Use only one of images, image, image_url or image_urls")
	}
	if len(references) > 0 {
		items := make([]map[string]string, 0, len(references))
		for _, reference := range references {
			if strings.TrimSpace(reference) == "" {
				return nil, imageLocalError(422, "invalid_reference_image", "Reference image must not be empty")
			}
			items = append(items, map[string]string{"image_url": reference})
		}
		fields["images"], _ = json.Marshal(items)
	}
	if aliases > 0 && !strings.Contains(c.Request.URL.Path, "/images/edits") {
		return nil, imageLocalError(422, "unsupported_parameter", "Use /images/edits/async for reference-image requests")
	}
	normalized, _ := json.Marshal(fields)
	parsed, err := s.ParseOpenAIImagesRequest(c, normalized)
	if err != nil {
		return nil, imageLocalError(400, "invalid_image_request", err.Error())
	}
	return parsed, nil
}
