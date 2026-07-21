// Package exportjson streams a tdl "chat export" JSON file.
//
// The file shape is:
//
//	{"id": <channelID>, "messages": [ {message}, {message}, ... ]}
//
// The messages array can be huge, so it is decoded element by element instead
// of loaded whole. Each message keeps its "raw" object verbatim for later media
// extraction.
package exportjson

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Message is the subset of a tdl-exported message the archiver needs. Text
// content is intentionally omitted: it already lives in the export file, which
// is the text archive.
type Message struct {
	ID   int             `json:"id"`
	Type string          `json:"type"`
	File string          `json:"file"`
	Date int             `json:"date"`
	Raw  json.RawMessage `json:"raw"`
}

// ParseFile opens path and streams it through Parse.
func ParseFile(path string, fn func(Message) error) (channelID int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return Parse(f, fn)
}

// Parse reads the export object, invoking fn for every element of the messages
// array in file order, and returns the channel id. fn is called after the id is
// known (tdl writes "id" before "messages"). A non-nil error from fn stops the
// stream and is returned.
func Parse(r io.Reader, fn func(Message) error) (channelID int64, err error) {
	dec := json.NewDecoder(r)

	// opening '{'
	if err := expectDelim(dec, '{'); err != nil {
		return 0, err
	}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return 0, fmt.Errorf("expected object key, got %T", keyTok)
		}

		switch key {
		case "id":
			if err := dec.Decode(&channelID); err != nil {
				return 0, fmt.Errorf("decode id: %w", err)
			}
		case "messages":
			if err := streamMessages(dec, fn); err != nil {
				return channelID, err
			}
		default:
			// consume and discard the value of any other key
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return channelID, fmt.Errorf("skip %q: %w", key, err)
			}
		}
	}

	return channelID, nil
}

func streamMessages(dec *json.Decoder, fn func(Message) error) error {
	if err := expectDelim(dec, '['); err != nil {
		return fmt.Errorf("messages: %w", err)
	}

	for dec.More() {
		var m Message
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("decode message: %w", err)
		}
		if err := fn(m); err != nil {
			return err
		}
	}

	// closing ']'
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

func expectDelim(dec *json.Decoder, want rune) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || rune(d) != want {
		return fmt.Errorf("expected %q, got %v", want, tok)
	}
	return nil
}
