package media

import (
	"encoding/json"
	"testing"
)

// These raw fixtures are trimmed from the actual output of
// json.Marshal(*tg.Message) as produced by tdl "chat export --raw"
// (gotd v0.140.0). Keep them realistic so the extractor stays honest.

const rawDocumentJSON = `{
  "ID": 123, "Date": 1700000000, "Message": "hello",
  "Media": {
    "Flags": 0, "Nopremium": false, "Spoiler": false,
    "Document": {
      "Flags": 0, "ID": 999, "AccessHash": 0, "FileReference": null,
      "Date": 0, "MimeType": "video/mp4", "Size": 456789,
      "Thumbs": null, "DCID": 0,
      "Attributes": [{"FileName": "clip.mp4"}]
    },
    "TTLSeconds": 0
  }
}`

const rawPhotoProgressiveJSON = `{
  "ID": 7, "Date": 0, "Message": "",
  "Media": {
    "Flags": 0, "Spoiler": false,
    "Photo": {
      "Flags": 0, "ID": 42, "AccessHash": 0, "FileReference": null, "Date": 0,
      "Sizes": [
        {"Type": "x", "W": 0, "H": 0, "Size": 1000},
        {"Type": "y", "W": 0, "H": 0, "Sizes": [100, 5000, 23456]}
      ],
      "VideoSizes": null, "DCID": 0
    },
    "TTLSeconds": 0
  }
}`

const rawPhotoPlainJSON = `{
  "ID": 8, "Media": {"Photo": {"ID": 55, "Sizes": [
    {"Type": "m", "Size": 500},
    {"Type": "x", "Size": 90000}
  ]}}
}`

const rawTextJSON = `{"ID": 1, "Message": "just text", "Media": null}`

const rawWebpageJSON = `{"ID": 2, "Media": {"Flags": 0, "Webpage": {"ID": 3}}}`

func TestExtractDocument(t *testing.T) {
	info, ok := Extract(json.RawMessage(rawDocumentJSON))
	if !ok {
		t.Fatal("expected media, got none")
	}
	if info.Type != TypeDocument {
		t.Errorf("type = %q, want document", info.Type)
	}
	if info.Size != 456789 {
		t.Errorf("size = %d, want 456789", info.Size)
	}
	if info.Name != "clip.mp4" {
		t.Errorf("name = %q, want clip.mp4", info.Name)
	}
}

func TestExtractPhotoProgressive(t *testing.T) {
	info, ok := Extract(json.RawMessage(rawPhotoProgressiveJSON))
	if !ok {
		t.Fatal("expected media, got none")
	}
	if info.Type != TypePhoto {
		t.Errorf("type = %q, want photo", info.Type)
	}
	// last size entry is progressive -> last of Sizes = 23456
	if info.Size != 23456 {
		t.Errorf("size = %d, want 23456", info.Size)
	}
	if info.Name != "42.jpg" {
		t.Errorf("name = %q, want 42.jpg", info.Name)
	}
}

func TestExtractPhotoPlain(t *testing.T) {
	info, ok := Extract(json.RawMessage(rawPhotoPlainJSON))
	if !ok {
		t.Fatal("expected media, got none")
	}
	// last entry is a plain PhotoSize -> 90000
	if info.Size != 90000 {
		t.Errorf("size = %d, want 90000", info.Size)
	}
}

func TestExtractDocumentNoFilename(t *testing.T) {
	const j = `{"Media": {"Document": {"ID": 777, "Size": 10, "MimeType": "application/octet-stream", "Attributes": [{"Duration": 5}]}}}`
	info, ok := Extract(json.RawMessage(j))
	if !ok {
		t.Fatal("expected media, got none")
	}
	if info.Name == "" {
		t.Error("name must be non-empty for the tdl -f media filter")
	}
	if info.Size != 10 {
		t.Errorf("size = %d, want 10", info.Size)
	}
}

func TestExtractSkipsNonMedia(t *testing.T) {
	for name, j := range map[string]string{
		"text":    rawTextJSON,
		"webpage": rawWebpageJSON,
		"empty":   ``,
		"null":    `null`,
	} {
		if _, ok := Extract(json.RawMessage(j)); ok {
			t.Errorf("%s: expected no media, got some", name)
		}
	}
}
