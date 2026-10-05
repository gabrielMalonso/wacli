package wa

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"go.mau.fi/whatsmeow"
)

func staticImageFixture(t *testing.T, format string) []byte {
	t.Helper()
	var data bytes.Buffer
	src := image.NewNRGBA(image.Rect(0, 0, 120, 60))
	src.Set(0, 0, color.NRGBA{R: 200, G: 100, A: 64})
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&data, src, nil)
	} else {
		err = png.Encode(&data, src)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func pngFixtureChunk(kind string, body []byte) []byte {
	value := make([]byte, len(body)+12)
	binary.BigEndian.PutUint32(value, uint32(len(body)))
	copy(value[4:], kind)
	copy(value[8:], body)
	binary.BigEndian.PutUint32(value[len(value)-4:], crc32.ChecksumIEEE(value[4:len(value)-4]))
	return value
}
func TestStaticImagePreparationAndFrozenEncoder(t *testing.T) {
	for _, format := range []string{"jpeg", "png"} {
		t.Run(format, func(t *testing.T) {
			data := staticImageFixture(t, format)
			original := bytes.Clone(data)
			meta, err := PrepareStaticImage(data)
			if err != nil || meta.Width != 120 || meta.Height != 60 || meta.MIME != "image/"+format {
				t.Fatal(meta, err)
			}
			// Golden measured with the unchanged base encoder at 92df3f9, including alpha.
			if format == "png" {
				digest := sha256.Sum256(meta.JPEGThumbnail)
				if hex.EncodeToString(digest[:]) != "e1810b9db7c6395ca37fe1168eee8eebcb93470528fca989a0c6bfb43f37dbe8" {
					t.Fatal("legacy alpha rendering changed")
				}
			}
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(meta.JPEGThumbnail))
			if err != nil || cfg.Width != 96 || cfg.Height != 48 {
				t.Fatal(cfg, err)
			}
			msg := BuildImageMessage(whatsmeow.UploadResponse{FileLength: uint64(len(data))}, " literal\\n\n ", meta)
			thumbnail := bytes.Clone(msg.GetJPEGThumbnail())
			meta.JPEGThumbnail[0] ^= 0xff
			if !bytes.Equal(msg.GetJPEGThumbnail(), thumbnail) || !bytes.Equal(data, original) || msg.GetCaption() != " literal\\n\n " {
				t.Fatal("mutated bytes or caption")
			}
		})
	}
}
func TestStaticImageRejectsMalformedAnimatedAndExcessive(t *testing.T) {
	pngData := staticImageFixture(t, "png")
	jpegData := staticImageFixture(t, "jpeg")
	oversized := bytes.Clone(pngData)
	binary.BigEndian.PutUint32(oversized[16:], 40_000_001)
	binary.BigEndian.PutUint32(oversized[20:], 1)
	binary.BigEndian.PutUint32(oversized[29:], crc32.ChecksumIEEE(oversized[12:29]))
	overflow := bytes.Clone(pngData)
	binary.BigEndian.PutUint32(overflow[16:], 0x7fffffff)
	binary.BigEndian.PutUint32(overflow[20:], 0x7fffffff)
	binary.BigEndian.PutUint32(overflow[29:], crc32.ChecksumIEEE(overflow[12:29]))
	cases := map[string][]byte{"text": []byte("image/png"), "truncated_png": pngData[:len(pngData)-4], "truncated_jpeg": jpegData[:len(jpegData)/2], "jpeg_missing_eoi": jpegData[:len(jpegData)-2], "pixel_limit": oversized, "overflow": overflow, "gif": []byte("GIF89a\x01\x00\x01\x00"), "png_trailing": append(bytes.Clone(pngData), 'x')}
	for _, kind := range []string{"acTL", "fcTL", "fdAT"} {
		cases[kind] = append(append(bytes.Clone(pngData[:33]), pngFixtureChunk(kind, make([]byte, 8))...), pngData[33:]...)
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareStaticImage(data); err == nil {
				t.Fatal("accepted invalid image")
			}
		})
	}
}

func TestStaticImageHeaderPixelBoundaryWithoutLargeDecode(t *testing.T) {
	data := staticImageFixture(t, "png")
	// Header/config inspection alone must accept the exact ceiling. The body
	// is deliberately tiny and is never decoded at this dimension.
	binary.BigEndian.PutUint32(data[16:], MaxStaticImagePixels)
	binary.BigEndian.PutUint32(data[20:], 1)
	binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))
	if cfg, _, err := staticImageConfig(data); err != nil || cfg.Width != MaxStaticImagePixels || cfg.Height != 1 {
		t.Fatal(cfg, err)
	}
}

func TestStaticImageEXIFOriginalAndEncodedOrientation(t *testing.T) {
	data := staticImageFixture(t, "jpeg")
	// Little-endian EXIF with orientation 6 (90 degrees), injected as APP1.
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0}
	segment := append([]byte{0xff, 0xe1, 0, byte(len(exif) + 2)}, exif...)
	data = append(append(append([]byte{}, data[:2]...), segment...), data[2:]...)
	original := bytes.Clone(data)
	meta, err := PrepareStaticImage(data)
	if err != nil || meta.Width != 120 || meta.Height != 60 || !bytes.Equal(data, original) {
		t.Fatal("original or encoded orientation changed", meta, err)
	}
	if err := ValidateStaticImageMetadata(data, meta); err != nil {
		t.Fatal(err)
	}
}
