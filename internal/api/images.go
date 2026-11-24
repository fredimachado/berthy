package api

import "berthy/internal/manifest"

type ImageLookup struct {
	Manifest manifest.Manifest `json:"manifest"`
	App      string            `json:"app"`
	Tag      string            `json:"tag,omitempty"`
}
type ResolvedImage struct {
	Image string `json:"image"`
	Tag   string `json:"tag"`
}
type ImageTags struct {
	Repository string   `json:"repository"`
	Tags       []string `json:"tags"`
}
