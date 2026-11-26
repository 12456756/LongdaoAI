package service

import "github.com/tidwall/gjson"

func sepHasImageOutputTool(body []byte) bool {
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("type").String() == "image_generation" {
			return true
		}
	}
	for _, mode := range gjson.GetBytes(body, "modalities").Array() {
		if mode.String() == "image" || mode.String() == "video" {
			return true
		}
	}
	return false
}
