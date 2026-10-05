package app

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestTranscriptionInputUsesConfinedObservedBytes(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("synthetic\x00fixture"), make([]byte, MaxTranscriptionInputBytes)} {
		loc := mediaArtifactFixture(t, "input")
		if err := os.WriteFile(loc.Path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReadTranscriptionInput(context.Background(), loc)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("input=%d: observed=%d, %v", len(data), len(got), err)
		}
	}
}

func TestTranscriptionInputRefusesMissingLargeChangedAndCancelled(t *testing.T) {
	for _, mode := range []string{"missing", "size", "directory", "changed", "cancel", "invalid_location"} {
		t.Run(mode, func(t *testing.T) {
			loc := mediaArtifactFixture(t, "input")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			code := TranscriptionPathNotAllowed
			switch mode {
			case "missing":
				code = TranscriptionInputNotFound
			case "size":
				f, err := os.Create(loc.Path)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(MaxTranscriptionInputBytes + 1); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
				code = TranscriptionInputTooLarge
			case "directory":
				if err := os.Mkdir(loc.Path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "invalid_location":
				loc.Check = nil
				code = TranscriptionInvalidArguments
			case "changed", "cancel":
				if err := os.WriteFile(loc.Path, []byte("synthetic"), 0o600); err != nil {
					t.Fatal(err)
				}
				checks := 0
				loc.Check = func() error {
					checks++
					if mode == "changed" && checks == 3 {
						return os.WriteFile(loc.Path, []byte("changed bytes"), 0o600)
					}
					if mode == "cancel" && checks == 2 {
						cancel()
					}
					return nil
				}
				if mode == "cancel" {
					code = TranscriptionCancelled
				}
			}
			data, err := ReadTranscriptionInput(ctx, loc)
			requireTranscriptionError(t, err, code)
			if data != nil {
				t.Fatal("failed read returned usable bytes")
			}
		})
	}
}
