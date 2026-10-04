package wa

import (
	"bytes"
	"io"
	"testing"
)

func TestHistoryAgentDiagnosticSink(t *testing.T) {
	var buf bytes.Buffer
	c := &Client{opts: Options{DiagnosticWriter: &buf}}
	logger := newWhatsmeowLogger("Client", "ERROR", c.diagnosticWriter())
	logger.Errorf("fixture operational detail")
	if buf.Len() == 0 {
		t.Fatal("diagnostic sink ignored")
	}
	c.opts.DiagnosticWriter = io.Discard
	logger = newWhatsmeowLogger("Client", "ERROR", c.diagnosticWriter())
	logger.Errorf("fixture silent operational detail")
	if bytes.Contains(buf.Bytes(), []byte("silent")) {
		t.Fatal("silent sink ignored")
	}
}
