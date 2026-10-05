package wa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.mau.fi/whatsmeow"
)

func TestAgentDirectMediaBytesAuthenticateBeforeFilesystemIO(t *testing.T) {
	for _, kind := range []struct {
		name    string
		keyType whatsmeow.MediaType
	}{{"audio", whatsmeow.MediaAudio}, {"document", whatsmeow.MediaDocument}, {"image", whatsmeow.MediaImage}} {
		t.Run(kind.name, func(t *testing.T) {
			plain := []byte("synthetic media bytes")
			key := bytes.Repeat([]byte{7}, 32)
			encrypted, encHash, hash := encryptedMediaFixture(t, plain, key, kind.keyType)
			served := encrypted
			status := http.StatusOK
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(status); _, _ = w.Write(served) }))
			defer server.Close()
			oldBase := directMediaBaseURL
			directMediaBaseURL = server.URL
			defer func() { directMediaBaseURL = oldBase }()
			got, err := DownloadMediaDirectBytes(context.Background(), "/fixture", encHash, hash, key, uint64(len(plain)), kind.name)
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("%q %v", got, err)
			}
			for _, tc := range []struct {
				name           string
				enc, hash, key []byte
				length         uint64
				status         int
				mutate         bool
				want           error
			}{
				{"missing hash", encHash, nil, key, 0, 200, false, ErrMediaMetadataInvalid},
				{"short hash", encHash, []byte{1}, key, 0, 200, false, ErrMediaMetadataInvalid},
				{"short key", encHash, hash, []byte{1}, 0, 200, false, ErrMediaMetadataInvalid},
				{"short cipher hash", []byte{1}, hash, key, 0, 200, false, ErrMediaMetadataInvalid},
				{"declared too large", encHash, hash, key, MaxMediaDownloadSize + 1, 200, false, ErrMediaTooLarge},
				{"MAC", nil, hash, key, 0, 200, true, whatsmeow.ErrInvalidMediaHMAC},
				{"cipher hash", encHash, hash, key, 0, 200, true, whatsmeow.ErrInvalidMediaEncSHA256},
				{"plaintext hash", encHash, bytes.Repeat([]byte{99}, sha256.Size), key, 0, 200, false, whatsmeow.ErrInvalidMediaSHA256},
				{"length", encHash, hash, key, 1, 200, false, whatsmeow.ErrFileLengthMismatch},
				{"403", encHash, hash, key, 0, 403, false, whatsmeow.ErrMediaDownloadFailedWith403},
				{"404", encHash, hash, key, 0, 404, false, whatsmeow.ErrMediaDownloadFailedWith404},
				{"410", encHash, hash, key, 0, 410, false, whatsmeow.ErrMediaDownloadFailedWith410},
			} {
				t.Run(tc.name, func(t *testing.T) {
					served = bytes.Clone(encrypted)
					status = tc.status
					if tc.mutate {
						served[0] ^= 1
					}
					before := calls
					_, err := DownloadMediaDirectBytes(context.Background(), "/fixture", tc.enc, tc.hash, tc.key, tc.length, kind.name)
					if !errors.Is(err, tc.want) {
						t.Fatalf("want %v got %v", tc.want, err)
					}
					if (errors.Is(tc.want, ErrMediaMetadataInvalid) || errors.Is(tc.want, ErrMediaTooLarge)) && calls != before {
						t.Fatal("invalid metadata reached HTTP")
					}
				})
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := DownloadMediaDirectBytes(ctx, "/fixture", encHash, hash, key, 0, kind.name); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel: %v", err)
			}
		})
	}
}

type zeroMediaReader struct{}

func (zeroMediaReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestAgentDirectMediaBytesBoundsUnknownLengthHTTPBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(w, zeroMediaReader{}, MaxMediaDownloadSize+maxEncryptedMediaDownloadOverhead+1)
	}))
	defer server.Close()
	old := directMediaBaseURL
	directMediaBaseURL = server.URL
	defer func() { directMediaBaseURL = old }()
	_, err := DownloadMediaDirectBytes(context.Background(), "/synthetic", nil, bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{7}, 32), 0, "audio")
	if !errors.Is(err, ErrMediaTooLarge) {
		t.Fatalf("unbounded response: %v", err)
	}
}
