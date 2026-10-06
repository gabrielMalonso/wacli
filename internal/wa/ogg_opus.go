package wa

import (
	"bytes"
	"context"
	"encoding/binary"
	"unicode/utf8"
)

const (
	OggOpusMIME       = "audio/ogg; codecs=opus"
	MaxOggOpusSamples = 172_800_000 // Local one-hour encoded timeline quota, not a WhatsApp limit.
)

// OggOpusMetadata describes declared structure, not decoded audio or quality.
// InputSampleRate is advisory; granules and EncodedSamples always use 48 kHz.
type OggOpusMetadata struct {
	OpusVersion, Channels, MappingFamily uint8
	PreSkip                              uint16
	InputSampleRate                      uint32
	OutputGain                           int16
	EncodedSamples, PlayableSamples      uint64
}

// OggOpusError contains only fixed categories/reasons, never tags or source paths.
type OggOpusError struct{ Code, Reason string }

func (e *OggOpusError) Error() string     { return "voice " + e.Code + ": " + e.Reason }
func opusError(code, reason string) error { return &OggOpusError{code, reason} }

type oggOpusLimits struct {
	file, tags, packet      int
	pages, packets, samples uint64
}

var voiceOggLimits = oggOpusLimits{100 << 20, 64 << 10, 61440, 1_000_000, 1_440_000, MaxOggOpusSamples}

// InspectOggOpus validates one complete, zero-origin Ogg Opus stream. It does
// not decode compressed frames, repair gaps, normalize bytes or run probes.
// RFC 3533 §6 and RFC 7845 §§3–5 define the container/header/timeline rules.
func InspectOggOpus(ctx context.Context, data []byte) (OggOpusMetadata, error) {
	return inspectOggOpus(ctx, data, voiceOggLimits)
}

func inspectOggOpus(ctx context.Context, data []byte, limits oggOpusLimits) (OggOpusMetadata, error) {
	var m OggOpusMetadata
	if err := ctx.Err(); err != nil {
		return m, err
	}
	if len(data) > limits.file {
		return m, opusError("quota", "local file byte limit exceeded")
	}
	var pages, packets, previousGranule uint64
	var serial, sequence uint32
	var packet []byte
	stage := 0 // Head, Tags, then audio. Only one bounded packet is assembled.
	firstAudio := true
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return OggOpusMetadata{}, err
		}
		if pages >= limits.pages {
			return OggOpusMetadata{}, opusError("quota", "local page count limit exceeded")
		}
		if len(data) < 27 || !bytes.Equal(data[:4], []byte("OggS")) {
			return OggOpusMetadata{}, opusError("invalid", "incomplete Ogg page or capture pattern")
		}
		if data[4] != 0 {
			return OggOpusMetadata{}, opusError("unsupported_profile", "Ogg structure version 0 required")
		}
		flags := data[5]
		if flags & ^byte(7) != 0 {
			return OggOpusMetadata{}, opusError("invalid", "reserved Ogg flags")
		}
		n := int(data[26])
		if n > len(data)-27 {
			return OggOpusMetadata{}, opusError("invalid", "incomplete lacing table")
		}
		laces := data[27 : 27+n]
		bodySize := 0
		for _, size := range laces {
			bodySize += int(size)
		} // At most 255*255.
		if bodySize > len(data)-27-n {
			return OggOpusMetadata{}, opusError("invalid", "incomplete page body")
		}
		page := data[:27+n+bodySize]
		if oggCRC(page) != binary.LittleEndian.Uint32(page[22:26]) {
			return OggOpusMetadata{}, opusError("invalid", "Ogg page CRC mismatch")
		}
		if n == 0 {
			return OggOpusMetadata{}, opusError("unsupported_profile", "empty Ogg pages are outside the voice profile")
		}
		pageSerial := binary.LittleEndian.Uint32(page[14:18])
		pageSequence := binary.LittleEndian.Uint32(page[18:22])
		if pages == 0 {
			serial = pageSerial
			if flags != 2 || pageSequence != 0 {
				return OggOpusMetadata{}, opusError("invalid", "initial BOS and sequence 0 required")
			}
		} else if pageSerial != serial || flags&2 != 0 {
			return OggOpusMetadata{}, opusError("unsupported_profile", "only one logical stream is supported")
		}
		if pageSequence != sequence {
			return OggOpusMetadata{}, opusError("invalid", "nonconsecutive page sequence")
		}
		sequence++ // The page quota precludes uint32 wrap.
		pages++
		if (len(packet) != 0) != (flags&1 != 0) {
			return OggOpusMetadata{}, opusError("invalid", "packet continuation flag mismatch")
		}
		headerPage := stage < 2
		body := page[27+n:]
		completed := 0
		var lastDuration uint64
		for i, size := range laces {
			if err := ctx.Err(); err != nil {
				return OggOpusMetadata{}, err
			}
			limit := limits.tags
			if stage == 2 {
				limit = limits.packet
			}
			if len(packet) > limit || int(size) > limit-len(packet) {
				return OggOpusMetadata{}, opusError("quota", "local header or audio packet byte limit exceeded")
			}
			packet = append(packet, body[:int(size)]...)
			body = body[int(size):]
			if size == 255 {
				continue
			}
			completed++
			switch stage {
			case 0:
				if pages != 1 || i != n-1 {
					return OggOpusMetadata{}, opusError("invalid", "OpusHead must finish alone on the BOS page")
				}
				var err error
				m, err = opusHead(packet)
				if err != nil {
					return OggOpusMetadata{}, err
				}
				stage = 1
			case 1:
				if i != n-1 {
					return OggOpusMetadata{}, opusError("invalid", "OpusTags must finish its own page")
				}
				if err := opusTags(packet); err != nil {
					return OggOpusMetadata{}, err
				}
				stage = 2
			case 2:
				if packets >= limits.packets {
					return OggOpusMetadata{}, opusError("quota", "local audio packet count limit exceeded")
				}
				duration, err := opusPacketSamples(packet)
				if err != nil {
					return OggOpusMetadata{}, err
				}
				if m.EncodedSamples > limits.samples || duration > limits.samples-m.EncodedSamples {
					return OggOpusMetadata{}, opusError("quota", "local one-hour encoded sample limit exceeded")
				}
				m.EncodedSamples += duration
				lastDuration = duration
				packets++
			}
			packet = packet[:0]
		}
		granule := binary.LittleEndian.Uint64(page[6:14])
		if completed == 0 {
			if granule != ^uint64(0) {
				return OggOpusMetadata{}, opusError("invalid", "unfinished page requires the absent granule sentinel")
			}
		} else if granule>>63 != 0 {
			return OggOpusMetadata{}, opusError("invalid", "completed page requires a nonnegative granule")
		} else if headerPage {
			if granule != 0 {
				return OggOpusMetadata{}, opusError("invalid", "completed header granule must be zero")
			}
		} else if flags&4 == 0 {
			if firstAudio && granule > m.EncodedSamples {
				return OggOpusMetadata{}, opusError("unsupported_profile", "cropped initial timeline is outside the zero-origin profile")
			}
			if granule != m.EncodedSamples {
				return OggOpusMetadata{}, opusError("invalid", "granule does not match completed packet durations")
			}
			previousGranule, firstAudio = granule, false
		}
		if pages == 1 && stage != 1 {
			return OggOpusMetadata{}, opusError("invalid", "OpusHead cannot span pages")
		}
		data = data[len(page):]
		if flags&4 != 0 {
			if headerPage || completed == 0 || len(packet) != 0 || packets == 0 {
				return OggOpusMetadata{}, opusError("invalid", "EOS requires completed audio without a pending fragment")
			}
			if firstAudio && granule > m.EncodedSamples {
				return OggOpusMetadata{}, opusError("unsupported_profile", "cropped initial timeline is outside the zero-origin profile")
			}
			// RFC 7845 §4.4: this profile trims at most the last packet.
			if granule > m.EncodedSamples || granule < previousGranule || granule < m.EncodedSamples-lastDuration || granule <= uint64(m.PreSkip) {
				return OggOpusMetadata{}, opusError("invalid", "inconsistent EOS trim or pre-skip")
			}
			if len(data) != 0 {
				if bytes.HasPrefix(data, []byte("OggS")) {
					return OggOpusMetadata{}, opusError("unsupported_profile", "chained streams are outside the voice profile")
				}
				return OggOpusMetadata{}, opusError("invalid", "bytes after EOS")
			}
			m.PlayableSamples = granule - uint64(m.PreSkip)
			return m, nil
		}
	}
	return OggOpusMetadata{}, opusError("invalid", "complete headers and EOS audio required")
}

func opusHead(p []byte) (OggOpusMetadata, error) {
	if len(p) < 8 || !bytes.Equal(p[:8], []byte("OpusHead")) {
		return OggOpusMetadata{}, opusError("unsupported_profile", "OpusHead identification required")
	}
	if len(p) < 19 {
		return OggOpusMetadata{}, opusError("invalid", "incomplete OpusHead")
	}
	if p[9] == 0 {
		return OggOpusMetadata{}, opusError("invalid", "zero output channels")
	}
	if p[8] != 1 || len(p) != 19 || p[18] != 0 {
		return OggOpusMetadata{}, opusError("unsupported_profile", "Opus version 1, 19-byte header and mapping family 0 required")
	}
	if p[9] > 2 {
		return OggOpusMetadata{}, opusError("invalid", "mapping family 0 requires one or two channels")
	}
	return OggOpusMetadata{OpusVersion: p[8], Channels: p[9], MappingFamily: p[18], PreSkip: binary.LittleEndian.Uint16(p[10:12]), InputSampleRate: binary.LittleEndian.Uint32(p[12:16]), OutputGain: int16(binary.LittleEndian.Uint16(p[16:18]))}, nil
}

func opusTags(p []byte) error {
	if len(p) < 16 || !bytes.Equal(p[:8], []byte("OpusTags")) {
		return opusError("invalid", "complete OpusTags header required")
	}
	p = p[8:]
	readString := func() bool {
		if len(p) < 4 {
			return false
		}
		n := uint64(binary.LittleEndian.Uint32(p[:4]))
		p = p[4:]
		if n > uint64(len(p)) || !utf8.Valid(p[:int(n)]) {
			return false
		}
		p = p[int(n):]
		return true
	}
	if !readString() || len(p) < 4 {
		return opusError("invalid", "invalid vendor string or comment count")
	}
	count := uint64(binary.LittleEndian.Uint32(p[:4]))
	p = p[4:]
	if count > uint64(len(p)/4) {
		return opusError("invalid", "comment count exceeds remaining bytes")
	}
	for range count {
		if !readString() {
			return opusError("invalid", "invalid comment string")
		}
	}
	// RFC 7845 §5.2 allows an opaque binary suffix. Preserve it in the original.
	return nil
}

// Only the framing in RFC 6716 §3 is inspected. Compressed frame bodies remain
// opaque. Empty frames are valid PLC/DTX; an empty packet is not.
func opusPacketSamples(p []byte) (uint64, error) {
	bad := func() (uint64, error) { return 0, opusError("invalid", "invalid Opus packet framing") }
	if len(p) == 0 {
		return bad()
	}
	config, code := p[0]>>3, p[0]&3
	var samples uint64
	switch {
	case config < 12:
		samples = [...]uint64{480, 960, 1920, 2880}[config&3]
	case config < 16:
		samples = [...]uint64{480, 960}[config&1]
	default:
		samples = [...]uint64{120, 240, 480, 960}[config&3]
	}
	p = p[1:]
	readSize := func() (int, bool) {
		if len(p) == 0 {
			return 0, false
		}
		n := int(p[0])
		p = p[1:]
		if n >= 252 {
			if len(p) == 0 {
				return 0, false
			}
			n += 4 * int(p[0])
			p = p[1:]
		}
		return n, true
	}
	frames := 1
	switch code {
	case 0:
		if len(p) > 1275 {
			return bad()
		}
	case 1:
		frames = 2
		if len(p)%2 != 0 || len(p)/2 > 1275 {
			return bad()
		}
	case 2:
		frames = 2
		n, ok := readSize()
		if !ok || n > len(p) || len(p)-n > 1275 {
			return bad()
		}
	case 3:
		if len(p) == 0 {
			return bad()
		}
		control := p[0]
		p = p[1:]
		frames = int(control & 63)
		if frames == 0 || uint64(frames)*samples > 5760 {
			return bad()
		}
		if control&64 != 0 {
			padding := 0
			for {
				if len(p) == 0 {
					return bad()
				}
				value := p[0]
				p = p[1:]
				n := int(value)
				if value == 255 {
					n = 254
				}
				if n > len(p)-padding {
					return bad()
				}
				padding += n
				if value != 255 {
					break
				}
			}
			p = p[:len(p)-padding]
		}
		if control&128 == 0 {
			if len(p)%frames != 0 || len(p)/frames > 1275 {
				return bad()
			}
		} else {
			total := 0
			for i := 0; i < frames-1; i++ {
				n, ok := readSize()
				if !ok || total > len(p) || n > len(p)-total {
					return bad()
				}
				total += n
			}
			if total > len(p) || len(p)-total > 1275 {
				return bad()
			}
		}
	}
	if uint64(frames)*samples > 5760 {
		return bad()
	}
	return uint64(frames) * samples, nil
}

// Ogg uses an unreflected CRC with init/final zero (RFC 3533 §6), unlike IEEE.
var oggCRCTable = func() [256]uint32 {
	var table [256]uint32
	for i := range table {
		value := uint32(i) << 24
		for range 8 {
			if value&0x80000000 != 0 {
				value = value<<1 ^ 0x04c11db7
			} else {
				value <<= 1
			}
		}
		table[i] = value
	}
	return table
}()

func oggCRC(page []byte) uint32 {
	var crc uint32
	for i, b := range page {
		if i >= 22 && i < 26 {
			b = 0
		}
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}
