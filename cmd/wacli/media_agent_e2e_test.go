package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/util/cbcutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
)

func encryptAgentMediaFixture(t *testing.T, data, key []byte, kind whatsmeow.MediaType) ([]byte, []byte, []byte) {
	t.Helper()
	expanded := hkdfutil.SHA256(key, nil, []byte(kind), 112)
	ciphertext, err := cbcutil.Encrypt(expanded[16:48], expanded[:16], bytes.Clone(data))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, expanded[48:80])
	_, _ = mac.Write(expanded[:16])
	_, _ = mac.Write(ciphertext)
	encrypted := append(ciphertext, mac.Sum(nil)[:10]...)
	encSum := sha256.Sum256(encrypted)
	sum := sha256.Sum256(data)
	return encrypted, encSum[:], sum[:]
}

// The executable keeps its production mmg.whatsapp.net URL. A fixture-only
// CONNECT proxy routes that host solely to a local TLS server with a test CA.
// No external dial or configurable production endpoint is introduced.
func mediaHTTPSFixture(t *testing.T, handler http.Handler) (proxyURL, caPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic media fixture"}, DNSNames: []string{"mmg.whatsapp.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	caPath = filepath.Join(t.TempDir(), "fixture-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "mmg.whatsapp.net:443" {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
			http.Error(w, "fixture rejects external traffic", http.StatusForbidden)
			return
		}
		upstream, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
		if err != nil {
			http.Error(w, "local fixture unavailable", http.StatusBadGateway)
			return
		}
		client, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		defer client.Close()
		defer upstream.Close()
		_, _ = fmt.Fprint(buf, "HTTP/1.1 200 Connection established\r\n\r\n")
		if err := buf.Flush(); err != nil {
			return
		}
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		_ = upstream.SetDeadline(time.Now().Add(10 * time.Second))
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = io.Copy(upstream, buf); _ = upstream.Close() })
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		wg.Wait()
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, caPath
}

func TestAgentMediaProductionBinaryHTTPSUnderOwnerLock(t *testing.T) {
	binary := os.Getenv("WACLI_MEDIA_E2E_BINARY")
	if binary == "" {
		t.Skip("set WACLI_MEDIA_E2E_BINARY to a freshly built production binary")
	}
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	data := []byte("synthetic decrypted audio")
	key := bytes.Repeat([]byte{7}, 32)
	encrypted, encHash, hash := encryptAgentMediaFixture(t, data, key, whatsmeow.MediaAudio)
	var httpCalls atomic.Int64
	proxy, ca := mediaHTTPSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		if r.Host != "mmg.whatsapp.net" {
			t.Errorf("host=%q", r.Host)
		}
		switch r.URL.Path {
		case "/fixture":
			_, _ = w.Write(encrypted)
		case "/tampered":
			corrupt := bytes.Clone(encrypted)
			corrupt[0] ^= 1
			_, _ = w.Write(corrupt)
		case "/expired":
			http.Error(w, "SECRET_URL_KEY", http.StatusGone)
		case "/transient":
			http.Error(w, "SECRET_URL_KEY", http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.Error(w, "fixture only", 404)
		}
	}))
	dir, _ := seedAgentMedia(t, "audio", false)
	db, err := store.Open(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id, path string
		digest   []byte
		length   uint64
	}{
		{"media-1", "/fixture", hash, uint64(len(data))},
		{"expired", "/expired", hash, uint64(len(data))},
		{"transient", "/transient", hash, uint64(len(data))},
		{"tampered", "/tampered", hash, uint64(len(data))},
		{"bad_plaintext", "/fixture", bytes.Repeat([]byte{99}, 32), uint64(len(data))},
		{"bad_length", "/fixture", hash, 1},
		{"too_large", "/fixture", hash, 100*1024*1024 + 1},
	} {
		if err := db.UpsertMessage(store.UpsertMessageParams{ChatJID: mediaFixtureChat, MsgID: row.id, Timestamp: time.Unix(1100, 0), MediaType: "audio", Filename: "fixture.ogg", MimeType: "audio/ogg", DirectPath: row.path, MediaKey: key, FileSHA256: row.digest, FileEncSHA256: encHash, FileLength: row.length}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	sessionSentinel := []byte("synthetic session sentinel: never a WA database")
	if err := os.WriteFile(filepath.Join(dir, "session.db"), sessionSentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	var ipcCalls atomic.Int64
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
		ipcCalls.Add(1)
		return sendDelegateResponse{}, fmt.Errorf("media must never use IPC")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// Snapshot DB, LOCK and directory modes without attempting to read a socket.
	before, err := os.ReadFile(filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	if err := os.Chmod(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(command, id, output string, extra ...string) (string, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		args := []string{"--store", dir, "--agent", "--timeout", "3s", "media", command, "--chat", mediaFixtureChat, "--id", id}
		if output != "" {
			args = append(args, "--output", output)
		}
		args = append(args, extra...)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), "HTTPS_PROXY="+proxy, "HTTP_PROXY="+proxy, "ALL_PROXY="+proxy, "NO_PROXY=localhost,127.0.0.1,::1", "SSL_CERT_FILE="+ca, "SSL_CERT_DIR="+filepath.Dir(ca), "WACLI_READONLY=0", "WACLI_MEDIA_ROOTS=")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	raw, stderr, err := run("status", "media-1", "", "--verify")
	if err != nil || stderr != "" || decodeMediaFixture(t, raw).Local.State != "absent" || httpCalls.Load() != 0 {
		t.Fatalf("status: %v %s %s", err, raw, stderr)
	}
	output := filepath.Join(outputDir, "audio.ogg")
	raw, stderr, err = run("download", "media-1", output)
	if err != nil || stderr != "" {
		t.Fatalf("download: %v %s %s", err, raw, stderr)
	}
	d := decodeMediaFixture(t, raw)
	if d.Status != "downloaded" || d.Recorded || d.Output == nil || !reflect.DeepEqual(d.Output.Checks, []string{"sha256", "declared_size", "hmac", "ciphertext_sha256"}) {
		t.Fatalf("%s", raw)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("%q %v", got, err)
	}
	st, err := os.Stat(outputDir)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("directory chmod: %v %v", st, err)
	}
	raw, stderr, err = run("download", "media-1", output, "--read-only")
	if err != nil || stderr != "" || decodeMediaFixture(t, raw).Status != "existing" || httpCalls.Load() != 1 {
		t.Fatalf("existing: %v %s %s", err, raw, stderr)
	}
	for _, tc := range []struct{ id, code string }{
		{"expired", "media_expired"}, {"transient", "download_failed"}, {"tampered", "integrity_failed"}, {"bad_plaintext", "integrity_failed"}, {"bad_length", "integrity_failed"}, {"too_large", "media_too_large"},
	} {
		out := filepath.Join(outputDir, tc.id)
		raw, stderr, err := run("download", tc.id, out)
		if err == nil || raw != "" || strings.Contains(stderr, "SECRET") || strings.Contains(stderr, "mmg.whatsapp.net") || strings.Contains(stderr, "direct_path") || strings.Contains(stderr, "media_key") {
			t.Fatalf("unsafe failure: %v %s %s", err, raw, stderr)
		}
		e := decodeAgentTest(t, stderr)
		if e.Error.Code != tc.code || e.Error.Media == nil || e.Error.Media.FilePublication != "not_written" {
			t.Fatalf("want %s: %s", tc.code, stderr)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("failure published %s: %v", out, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, "wacli.db"))
	if err != nil || !bytes.Equal(before, after) || ipcCalls.Load() != 0 {
		t.Fatalf("archive/IPC mutation: %v calls=%d", err, ipcCalls.Load())
	}
	if got, err := os.ReadFile(filepath.Join(dir, "session.db")); err != nil || !bytes.Equal(got, sessionSentinel) {
		t.Fatalf("session sentinel changed: %v", err)
	}
}
