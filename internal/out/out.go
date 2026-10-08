package out

import (
	"encoding/json"
	"fmt"
	"io"
)

type envelope struct {
	Success bool    `json:"success"`
	Data    any     `json:"data"`
	Error   *string `json:"error"`
}

func WriteJSON(w io.Writer, data any) error {
	return writeJSON(w, data, true)
}

// WriteActionJSON preserves output failure after an action has taken effect.
func WriteActionJSON(w io.Writer, data any) error {
	return writeJSON(w, data, false)
}

func writeJSON(w io.Writer, data any, ignoreBrokenPipe bool) error {
	b, err := json.Marshal(envelope{Success: true, Data: data})
	if err != nil {
		return err
	}
	n, err := fmt.Fprintln(w, string(b))
	if err == nil && n != len(b)+1 {
		err = io.ErrShortWrite
	}
	if ignoreBrokenPipe && isPlatformBrokenPipe(err) {
		return nil
	}
	return err
}

func WriteError(w io.Writer, asJSON bool, err error) error {
	if err == nil {
		return nil
	}
	if asJSON {
		msg := err.Error()
		b, _ := json.Marshal(envelope{Success: false, Data: nil, Error: &msg})
		_, _ = fmt.Fprintln(w, string(b))
		return nil
	}
	_, _ = fmt.Fprintln(w, SanitizeHuman(err.Error()))
	return nil
}
