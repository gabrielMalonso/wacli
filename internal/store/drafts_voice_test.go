package store

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func draftVoiceFixture() DraftPayloadData {
	data := draftTextFixture("unused")
	data.Text = nil
	data.Kind = DraftVoiceKind
	data.Voice = &DraftVoice{MIME: "audio/ogg; codecs=opus", Size: 131, SHA256: "3fe162b231f1d159cad1d48773e55a3e038668682920472a987527879c02da7a", OpusVersion: 1, Channels: 1, InputSampleRate: 48000, EncodedSamples: 960, PlayableSamples: 960}
	return data
}
func TestDraftVoiceCanonicalGoldenAndOldImage(t *testing.T) {
	p := mustDraftPayload(t, draftVoiceFixture())
	const want = `{"version":1,"account":{"pn":"15550000001@s.whatsapp.net","lid":""},"recipient":{"jid":"15550000002@s.whatsapp.net","pn":"15550000002@s.whatsapp.net","lid":""},"kind":"voice","defaults":{"link_preview":false,"ephemeral":false,"expiration":0,"allow_self":false},"voice":{"mime":"audio/ogg; codecs=opus","size":131,"sha256":"3fe162b231f1d159cad1d48773e55a3e038668682920472a987527879c02da7a","opus_version":1,"channels":1,"mapping_family":0,"pre_skip":0,"input_sample_rate":48000,"output_gain":0,"encoded_samples":960,"playable_samples":960}}`
	const hash = "0f122d04d854eeb30699e78f17b8cd8ef0f8e33bee0aecf681c2f3e5e8ccb23e"
	if string(p.CanonicalJSON()) != want || p.Hash() != hash {
		t.Fatal("voice golden", p.Hash(), string(p.CanonicalJSON()))
	}
	if q, err := DecodeDraftPayload([]byte(want)); err != nil || q.Hash() != hash {
		t.Fatal(err)
	}
	// Independently captured from unchanged image main0f74 before voice edits.
	// Existing text/document/contact tests pin their original canonical bytes.
	image := mustDraftPayload(t, draftImageFixture(t))
	if image.Hash() != "278f16fd5b7dd2686fc4394d6c629ca57e92833d08069e2411a91ecc474e3a85" {
		t.Fatal("image canonical/hash changed", image.Hash())
	}
}
func TestDraftVoiceCopiesMetadataHashAndInvalidUnion(t *testing.T) {
	data := draftVoiceFixture()
	p := mustDraftPayload(t, data)
	before := p.CanonicalJSON()
	data.Voice.InputSampleRate++
	copy := p.Data()
	copy.Voice.Channels = 2
	if !bytes.Equal(before, p.CanonicalJSON()) {
		t.Fatal("voice metadata aliases")
	}
	for _, mutate := range []func(*DraftVoice){func(v *DraftVoice) { v.Size++ }, func(v *DraftVoice) { v.SHA256 = strings.Repeat("a", 64) }, func(v *DraftVoice) { v.Channels = 2 }, func(v *DraftVoice) { v.InputSampleRate = 0 }, func(v *DraftVoice) { v.OutputGain = -256 }, func(v *DraftVoice) { v.PreSkip = 1; v.PlayableSamples-- }, func(v *DraftVoice) { v.EncodedSamples += 120 }, func(v *DraftVoice) { v.PlayableSamples-- }} {
		d := p.Data()
		mutate(d.Voice)
		q, err := NewDraftPayload(d)
		if err != nil || q.Hash() == p.Hash() {
			t.Fatal("frozen metadata not hashed", err)
		}
	}
	for _, mutate := range []func(*DraftVoice){func(v *DraftVoice) { v.MIME = "audio/mpeg" }, func(v *DraftVoice) { v.Size = 0 }, func(v *DraftVoice) { v.Size = MaxDraftFileBytes + 1 }, func(v *DraftVoice) { v.SHA256 = "bad" }, func(v *DraftVoice) { v.OpusVersion = 2 }, func(v *DraftVoice) { v.MappingFamily = 1 }, func(v *DraftVoice) { v.Channels = 0 }, func(v *DraftVoice) { v.Channels = 3 }, func(v *DraftVoice) { v.EncodedSamples = MaxDraftVoiceSamples + 120 }, func(v *DraftVoice) { v.EncodedSamples = 961 }, func(v *DraftVoice) { v.PlayableSamples = 0 }, func(v *DraftVoice) { v.PreSkip = 961 }, func(v *DraftVoice) { v.PlayableSamples = 961 }} {
		d := p.Data()
		mutate(d.Voice)
		if _, err := NewDraftPayload(d); err == nil {
			t.Fatal("invalid voice accepted")
		}
	}
	d := p.Data()
	d.Text = &DraftText{Text: "text"}
	if _, err := NewDraftPayload(d); err == nil {
		t.Fatal("union accepted")
	}
	for _, field := range []string{`"waveform":"AA=="`, `"seconds":1`, `"caption":"invented"`, `"source_path":"private"`} {
		raw := strings.Replace(string(before), `"playable_samples":960`, `"playable_samples":960,`+field, 1)
		if _, err := DecodeDraftPayload([]byte(raw)); err == nil {
			t.Fatal("unexpected voice field ignored", field)
		}
	}
}
func TestDraftVoiceReviewAndCleanupRemainNonDocument(t *testing.T) {
	p := mustDraftPayload(t, draftVoiceFixture())
	now := time.Unix(1700000000, 0)
	id, rid := strings.Repeat("1", 32), strings.Repeat("2", 32)
	path, _ := DraftSnapshotRelativePath(rid)
	if _, err := NewDraftRevision(id, rid, now, p, DraftReviewSnapshot{RequestedRaw: p.Data().Recipient.JID}); err == nil {
		t.Fatal("voice review without snapshot")
	}
	rev, err := NewDraftRevision(id, rid, now, p, DraftReviewSnapshot{RequestedRaw: p.Data().Recipient.JID, SnapshotPath: path, VerifiedAtCreate: now})
	if err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t)
	entry, err := db.WriteDraft(t.Context(), rev, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DiscardDraft(t.Context(), id, rid); err != nil {
		t.Fatal(err)
	}
	entry.Record.State = "discarded"
	if draftCleanupEligibility(entry, false) != DraftCleanupNonDocument || draftCleanupEligibility(entry, true) != DraftCleanupNonDocument {
		t.Fatal("voice cleanup broadened")
	}
	if summary := DraftRevisionSummary(rev); summary.Kind != DraftVoiceKind || summary.Preview != "" || summary.TextBytes != 0 {
		t.Fatal("spoken text invented", summary)
	}
}
