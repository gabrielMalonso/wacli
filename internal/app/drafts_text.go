package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/store"
)

// ReadDraftText applies the same limit to files and stdin without decoding
// escapes, trimming, normalizing Unicode or changing newline bytes.
func ReadDraftText(ctx context.Context, reader io.Reader) (string, error) {
	var buffer bytes.Buffer
	if _, err := copyDraftSnapshotBytes(ctx, &buffer, reader, store.MaxDraftFieldBytes); err != nil {
		return "", err
	}
	if !utf8.Valid(buffer.Bytes()) {
		return "", fmt.Errorf("draft text must be valid UTF-8")
	}
	return buffer.String(), nil
}
