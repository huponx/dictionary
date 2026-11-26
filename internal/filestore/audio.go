package filestore

import (
	"bytes"
	"errors"
)

var (
	// ErrAudioFormat: not an mp3, ogg, wav or m4a file.
	ErrAudioFormat = errors.New("filestore: not a supported audio file")
	// ErrAudioTooLarge: the file is larger than the limit.
	ErrAudioTooLarge = errors.New("filestore: audio is too large")
)

// SniffAudio returns the content type of an audio file by its magic bytes (audio/mpeg, audio/ogg, audio/wav
// or audio/mp4 for m4a), or ErrAudioFormat; ErrAudioTooLarge when it is longer than maxBytes. The bytes are
// stored as they are: the file is not decoded or re-encoded.
func SniffAudio(b []byte, maxBytes int) (string, error) {
	var ct string
	switch {
	case bytes.HasPrefix(b, []byte("ID3")), len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		ct = "audio/mpeg"
	case bytes.HasPrefix(b, []byte("OggS")):
		ct = "audio/ogg"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WAVE")):
		ct = "audio/wav"
	case len(b) >= 12 && bytes.Equal(b[4:8], []byte("ftyp")):
		ct = "audio/mp4"
	default:
		return "", ErrAudioFormat
	}
	if maxBytes > 0 && len(b) > maxBytes {
		return "", ErrAudioTooLarge
	}
	return ct, nil
}
