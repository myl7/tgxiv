// Package media extracts downloadable-media info (size, name, type) from the
// "raw" field of a tdl "chat export --raw" JSON message.
//
// tdl writes the raw message with the standard library json.Marshal on a
// gotd *tg.Message. gotd tg types carry no json tags and no custom marshaler,
// so the raw object uses Go field names (CamelCase) and, because the media
// field is an interface with no type discriminator, the concrete media type is
// told apart only by which sub-object is present ("Document" vs "Photo").
//
// The size logic mirrors tdl's core/tmedia so ordering and verification use the
// exact byte count tdl itself would report:
//   - document: Media.Document.Size
//   - photo:    last of Media.Photo.Sizes; PhotoSize -> .Size,
//     PhotoSizeProgressive -> last of .Sizes
package media

import (
	"encoding/json"
	"strconv"
)

// Type is the kind of downloadable media.
type Type string

const (
	TypeDocument Type = "document"
	TypePhoto    Type = "photo"
)

// Info is the extracted, download-relevant view of a message's media.
type Info struct {
	Type Type
	Size int64  // expected size in bytes, used for ordering and verification
	Name string // best-effort file name, only needs to be non-empty for tdl -f
}

// rawMessage is the subset of the marshaled *tg.Message we care about.
type rawMessage struct {
	Media *rawMedia `json:"Media"`
}

type rawMedia struct {
	Document *rawDocument `json:"Document"`
	Photo    *rawPhoto    `json:"Photo"`
}

type rawDocument struct {
	ID         int64             `json:"ID"`
	Size       int64             `json:"Size"`
	MimeType   string            `json:"MimeType"`
	Attributes []rawDocAttribute `json:"Attributes"`
}

type rawDocAttribute struct {
	FileName string `json:"FileName"`
}

type rawPhoto struct {
	ID    int64          `json:"ID"`
	Sizes []rawPhotoSize `json:"Sizes"`
}

// rawPhotoSize covers both tg.PhotoSize (Size) and tg.PhotoSizeProgressive
// (Sizes). Only one of the two is populated for a given entry.
type rawPhotoSize struct {
	Type  string  `json:"Type"`
	Size  int64   `json:"Size"`
	Sizes []int64 `json:"Sizes"`
}

// Extract returns the downloadable-media info for a raw message, or ok=false
// when the message carries no media we can download (text, webpage, poll, geo,
// empty raw, ...). Behavior matches tdl: unknown/absent media is skipped.
func Extract(raw json.RawMessage) (Info, bool) {
	if len(raw) == 0 {
		return Info{}, false
	}

	var m rawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return Info{}, false
	}
	if m.Media == nil {
		return Info{}, false
	}

	switch {
	case m.Media.Document != nil:
		return documentInfo(m.Media.Document), true
	case m.Media.Photo != nil:
		info, ok := photoInfo(m.Media.Photo)
		return info, ok
	default:
		return Info{}, false
	}
}

func documentInfo(d *rawDocument) Info {
	name := ""
	for _, attr := range d.Attributes {
		if attr.FileName != "" {
			name = attr.FileName
			break
		}
	}
	if name == "" {
		// tdl falls back to "<id><ext>"; we only need a non-empty name for the
		// tdl -f media filter, so a stable placeholder is enough.
		name = strconv.FormatInt(d.ID, 10) + ".bin"
	}
	return Info{Type: TypeDocument, Size: d.Size, Name: name}
}

func photoInfo(p *rawPhoto) (Info, bool) {
	if len(p.Sizes) == 0 {
		return Info{}, false
	}

	// tdl uses the last size entry unconditionally (core/tmedia GetPhotoSize).
	last := p.Sizes[len(p.Sizes)-1]
	var size int64
	switch {
	case len(last.Sizes) > 0: // PhotoSizeProgressive
		size = last.Sizes[len(last.Sizes)-1]
	case last.Size > 0: // PhotoSize
		size = last.Size
	default:
		return Info{}, false
	}

	// Telegram photos are always jpg; name mirrors tdl's "<photoID>.jpg".
	return Info{
		Type: TypePhoto,
		Size: size,
		Name: strconv.FormatInt(p.ID, 10) + ".jpg",
	}, true
}
