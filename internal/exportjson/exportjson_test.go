package exportjson

import (
	"strings"
	"testing"
)

// mirrors tdl "chat export --all --with-content --raw" output shape
const sample = `{
  "id": 1234567890,
  "messages": [
    {"id": 10, "type": "message", "file": "a.mp4", "date": 1700000000, "text": "with media",
     "raw": {"ID": 10, "Media": {"Document": {"ID": 1, "Size": 100}}}},
    {"id": 11, "type": "message", "file": "", "date": 1700000100, "text": "text only",
     "raw": {"ID": 11, "Message": "text only", "Media": null}},
    {"id": 12, "type": "service", "file": "", "date": 1700000200,
     "raw": {"ID": 12}},
    {"id": 13, "type": "message", "file": "b.jpg", "date": 1700000300,
     "raw": {"ID": 13, "Media": {"Photo": {"ID": 9, "Sizes": [{"Type": "x", "Size": 5000}]}}}}
  ]
}`

func TestParse(t *testing.T) {
	var got []Message
	channelID, err := Parse(strings.NewReader(sample), func(m Message) error {
		got = append(got, m)
		return nil
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if channelID != 1234567890 {
		t.Errorf("channelID = %d, want 1234567890", channelID)
	}
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4", len(got))
	}
	if got[0].ID != 10 || got[0].File != "a.mp4" || got[0].Date != 1700000000 {
		t.Errorf("msg0 = %+v", got[0])
	}
	if got[0].Text != "with media" {
		t.Errorf("msg0 text = %q, want %q", got[0].Text, "with media")
	}
	if len(got[0].Raw) == 0 {
		t.Error("msg0 raw should be captured")
	}
	if got[1].Text != "text only" {
		t.Errorf("msg1 text = %q, want %q", got[1].Text, "text only")
	}
	// tdl omits empty text (omitempty), so service messages decode to ""
	if got[2].Text != "" {
		t.Errorf("msg2 text = %q, want empty", got[2].Text)
	}
	if got[3].ID != 13 || got[3].Type != "message" {
		t.Errorf("msg3 = %+v", got[3])
	}
}

func TestParseStopsOnError(t *testing.T) {
	count := 0
	_, err := Parse(strings.NewReader(sample), func(m Message) error {
		count++
		if count == 2 {
			return errStop
		}
		return nil
	})
	if err != errStop {
		t.Errorf("err = %v, want errStop", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (stream should stop)", count)
	}
}

var errStop = stopError("stop")

type stopError string

func (e stopError) Error() string { return string(e) }
