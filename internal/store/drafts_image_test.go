package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/jpeg"
	"strings"
	"testing"
	"time"
)

func draftImageFixture(t *testing.T) DraftPayloadData {
	t.Helper()
	var thumb bytes.Buffer
	if err := jpeg.Encode(&thumb, image.NewRGBA(image.Rect(0, 0, 2, 1)), nil); err != nil {
		t.Fatal(err)
	}
	data := draftTextFixture("unused")
	data.Text = nil
	data.Kind = DraftImageKind
	data.Image = &DraftImage{MIME: "image/png", Caption: " raw\\n\n ", Size: 10, SHA256: strings.Repeat("a", 64), Width: 2, Height: 1, JPEGThumbnail: thumb.Bytes()}
	return data
}
func TestDraftImageHashCopiesValidationAndRetention(t *testing.T) {
	data := draftImageFixture(t)
	p := mustDraftPayload(t, data)
	original := p.CanonicalJSON()
	data.Image.JPEGThumbnail[0] ^= 0xff
	copy := p.Data()
	copy.Image.JPEGThumbnail[0] ^= 0xff
	if !bytes.Equal(original, p.CanonicalJSON()) {
		t.Fatal("mutable image payload")
	}
	for _, mutate := range []func(*DraftImage){func(v *DraftImage) { v.Caption += "x" }, func(v *DraftImage) { v.MIME = "image/jpeg" }, func(v *DraftImage) { v.Width++ }, func(v *DraftImage) { v.Size++ }, func(v *DraftImage) { v.SHA256 = strings.Repeat("b", 64) }, func(v *DraftImage) { v.JPEGThumbnail = append(v.JPEGThumbnail, 0) }} {
		value := p.Data()
		mutate(value.Image)
		changed, err := NewDraftPayload(value)
		if err != nil || changed.Hash() == p.Hash() {
			t.Fatal("effective metadata not hashed", err)
		}
	}
	for _, mutate := range []func(*DraftImage){func(v *DraftImage) { v.MIME = "image/gif" }, func(v *DraftImage) { v.Width = 0 }, func(v *DraftImage) { v.Width = ^uint32(0); v.Height = ^uint32(0) }, func(v *DraftImage) { v.JPEGThumbnail = nil }, func(v *DraftImage) { v.Size = MaxDraftFileBytes + 1 }} {
		value := p.Data()
		mutate(value.Image)
		if _, err := NewDraftPayload(value); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	value := p.Data()
	value.Document = &DraftDocument{}
	if _, err := NewDraftPayload(value); err == nil {
		t.Fatal("ambiguous union")
	}
	now := time.Now().UTC()
	id, rid := strings.Repeat("1", 32), strings.Repeat("2", 32)
	path, _ := DraftSnapshotRelativePath(rid)
	rev, err := NewDraftRevision(id, rid, now, p, DraftReviewSnapshot{RequestedRaw: p.Data().Recipient.JID, SnapshotPath: path, VerifiedAtCreate: now})
	if err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t)
	entry, err := db.WriteDraft(t.Context(), rev, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.DiscardDraft(t.Context(), id, rid)
	if err != nil {
		t.Fatal(err)
	}
	entry.Record.State = "discarded"
	if draftCleanupEligibility(entry, false) != DraftCleanupNonDocument {
		t.Fatal("image became deletable")
	}
}
func TestDraftOldVariantCanonicalImageAddition(t *testing.T) {
	// Freeze independent canonical values from the base commit, including omitted
	// image data. A new union must not rewrite any retained hash.
	cases := []string{
		`{"version":1,"account":{"pn":"15550000001@s.whatsapp.net","lid":""},"recipient":{"jid":"15550000002@s.whatsapp.net","pn":"15550000002@s.whatsapp.net","lid":""},"kind":"document","defaults":{"link_preview":false,"ephemeral":false,"expiration":0,"allow_self":false},"document":{"filename":"f.png","mime":"image/png","caption":"literal","size":10,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		`{"version":1,"account":{"pn":"15550000001@s.whatsapp.net","lid":""},"recipient":{"jid":"15550000002@s.whatsapp.net","pn":"15550000002@s.whatsapp.net","lid":""},"kind":"contact","defaults":{"link_preview":false,"ephemeral":false,"expiration":0,"allow_self":false},"contact":{"display_name":"Fixture","phone":"15550000003","vcard":"BEGIN:VCARD\r\nVERSION:3.0\r\nN:;Fixture;;;\r\nFN:Fixture\r\nTEL;TYPE=CELL;waid=15550000003:+15550000003\r\nEND:VCARD\r\n"}}`,
	}
	for _, raw := range cases {
		p, err := DecodeDraftPayload([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(append([]byte("wacli-draft-payload-v1\x00"), []byte(raw)...))
		if p.Hash() != hex.EncodeToString(sum[:]) || !bytes.Equal(p.CanonicalJSON(), []byte(raw)) {
			t.Fatal("legacy canonical changed")
		}
	}
}
