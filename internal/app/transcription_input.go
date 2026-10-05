package app

import (
	"context"
	"errors"
	"io"
	"os"
)

// ReadTranscriptionInput opens one confined regular file once. The cap and
// context are checked between bounded reads, and PR17 FD/path identity checks
// bracket IO. Observed bytes are not a promise of an immutable source file or
// an OS sandbox against concurrent writers. No App/archive/session is opened.
func ReadTranscriptionInput(ctx context.Context, loc MediaLocation) ([]byte, error) {
	if loc.Root == nil || loc.Check == nil {
		return nil, transcriptionFailure(TranscriptionInvalidArguments, nil)
	}
	if err := transcriptionContextError(ctx); err != nil {
		return nil, err
	}
	if err := loc.Check(); err != nil {
		return nil, transcriptionFailure(TranscriptionPathNotAllowed, err)
	}
	info, err := loc.Root.Lstat(loc.Name)
	if err != nil {
		return nil, transcriptionInputError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, transcriptionFailure(TranscriptionPathNotAllowed, nil)
	}
	if info.Size() > MaxTranscriptionInputBytes {
		return nil, transcriptionFailure(TranscriptionInputTooLarge, nil)
	}
	f, err := loc.Root.OpenFile(loc.Name, outboundReadFlags(), 0)
	if err != nil {
		return nil, transcriptionInputError(err)
	}
	defer f.Close()
	if err := checkMediaFile(loc, f, info); err != nil {
		return nil, transcriptionFailure(TranscriptionPathNotAllowed, err)
	}
	data := make([]byte, 0, int(info.Size()))
	var chunk [32 << 10]byte
	for {
		if err := transcriptionContextError(ctx); err != nil {
			return nil, err
		}
		n, err := f.Read(chunk[:min(len(chunk), MaxTranscriptionInputBytes-len(data)+1)])
		if ctxErr := transcriptionContextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		if len(data)+n > MaxTranscriptionInputBytes {
			return nil, transcriptionFailure(TranscriptionInputTooLarge, nil)
		}
		data = append(data, chunk[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, transcriptionInputError(err)
		}
		if n == 0 {
			return nil, transcriptionInputError(io.ErrNoProgress)
		}
	}
	if err := checkMediaFile(loc, f, info); err != nil {
		return nil, transcriptionFailure(TranscriptionPathNotAllowed, err)
	}
	return data, nil
}

func transcriptionInputError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return transcriptionFailure(TranscriptionInputNotFound, err)
	}
	return transcriptionFailure(TranscriptionInputUnreadable, err)
}
