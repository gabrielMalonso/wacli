// Synthetic offline protocol fixture, never a speech recognition provider.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 5 || os.Args[1] != "--protocol" || os.Args[2] != "wacli-transcribe-v1" || os.Args[3] != "--mime-type" {
		os.Exit(81)
	}
	_, parameters, err := mime.ParseMediaType(os.Args[4])
	if err != nil {
		os.Exit(82)
	}
	if marker := os.Getenv("WACLI_TRANSCRIBE_STUB_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte("executed"), 0o600); err != nil {
			os.Exit(83)
		}
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, (25<<20)+1))
	if err != nil || len(input) > 25<<20 {
		os.Exit(84)
	}
	digest := sha256.Sum256(input)
	text := fmt.Sprintf("%d:%s:%s", len(input), hex.EncodeToString(digest[:]), os.Args[4])
	var language *string
	pt := "pt-BR"
	language = &pt
	switch parameters["mode"] {
	case "empty":
		text, language = "", nil
	case "long":
		text = strings.Repeat("界", 400)
	case "block":
		time.Sleep(10 * time.Second)
	case "nonzero":
		fmt.Fprint(os.Stderr, strings.Repeat("PRIVATE_STDERR_SECRET_URL_PATH_KEY", 10000))
		os.Exit(85)
	case "unknown":
		fmt.Fprint(os.Stdout, `{"schema_version":1,"text":"PRIVATE_STDOUT_SECRET","secret":true}`)
		return
	case "trailing":
		fmt.Fprint(os.Stdout, `{"schema_version":1,"text":"PRIVATE_STDOUT_SECRET"} {}`)
		return
	case "duplicate":
		fmt.Fprint(os.Stdout, `{"schema_version":1,"text":"PRIVATE_STDOUT_SECRET","text":"other"}`)
		return
	case "null":
		fmt.Fprint(os.Stdout, `{"schema_version":1,"text":null}`)
		return
	case "version":
		fmt.Fprint(os.Stdout, `{"schema_version":2,"text":"PRIVATE_STDOUT_SECRET"}`)
		return
	case "utf8":
		_, _ = os.Stdout.Write(append([]byte(`{"schema_version":1,"text":"`), 255, '"', '}'))
		return
	case "oversize":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte{'x'}, (256<<10)+1))
		return
	case "textoversize":
		text = strings.Repeat("x", (128<<10)+1)
	}
	output := struct {
		SchemaVersion int     `json:"schema_version"`
		Text          string  `json:"text"`
		Language      *string `json:"language,omitempty"`
	}{1, text, language}
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		os.Exit(86)
	}
}
