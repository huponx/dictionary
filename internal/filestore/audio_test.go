package filestore

import (
	"errors"
	"testing"
)

func TestSniffAudio(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"mp3 id3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00abc"), "audio/mpeg"},
		{"mp3 frame", []byte{0xFF, 0xFB, 0x90, 0x00, 0x00}, "audio/mpeg"},
		{"ogg", []byte("OggS\x00\x02abcdefgh"), "audio/ogg"},
		{"wav", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), "audio/wav"},
		{"m4a", []byte("\x00\x00\x00\x20ftypM4A \x00\x00"), "audio/mp4"},
	}
	for _, c := range cases {
		got, err := SniffAudio(c.data, 1<<10)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
		if e := Ext(got); ContentType(e) != got {
			t.Errorf("%s: ext %q does not map back to %q", c.name, e, got)
		}
	}
}

func TestSniffAudioRefuses(t *testing.T) {
	for name, data := range map[string][]byte{
		"png":   []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x00"),
		"text":  []byte("hello world, not audio"),
		"webp":  []byte("RIFF\x24\x00\x00\x00WEBPVP8 "),
		"empty": nil,
		"short": {0xFF},
	} {
		if _, err := SniffAudio(data, 1<<10); !errors.Is(err, ErrAudioFormat) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := SniffAudio(append([]byte("ID3"), make([]byte, 100)...), 50); !errors.Is(err, ErrAudioTooLarge) {
		t.Fatalf("got %v", err)
	}
}
