package wa

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

const ImageThumbnailMaxDimension = 96
const MaxStaticImagePixels = 40_000_000

// ImageMetadata describes the bytes inspected during preparation. Dispatch
// uses these frozen values without decoding the original or rebuilding a preview.
type ImageMetadata struct {
	MIME          string
	Width, Height uint32
	JPEGThumbnail []byte
}

func PrepareStaticImage(data []byte) (ImageMetadata, error) {
	cfg, format, err := staticImageConfig(data)
	if err != nil {
		return ImageMetadata{}, err
	}
	src, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format || src.Bounds().Dx() != cfg.Width || src.Bounds().Dy() != cfg.Height {
		return ImageMetadata{}, fmt.Errorf("image cannot be fully decoded")
	}
	thumbnail, err := imageThumbnail(src)
	if err != nil {
		return ImageMetadata{}, fmt.Errorf("image thumbnail cannot be encoded")
	}
	return ImageMetadata{"image/" + format, uint32(cfg.Width), uint32(cfg.Height), thumbnail}, nil
}

// ValidateStaticImageMetadata checks the frozen fields against the verified
// upload buffer's header. It never decodes pixels or rebuilds the thumbnail.
func ValidateStaticImageMetadata(data []byte, meta ImageMetadata) error {
	cfg, format, err := staticImageConfig(data)
	if err != nil {
		return err
	}
	if meta.MIME != "image/"+format || meta.Width != uint32(cfg.Width) || meta.Height != uint32(cfg.Height) {
		return fmt.Errorf("image metadata does not match snapshot header")
	}
	return nil
}

func staticImageConfig(data []byte) (image.Config, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return image.Config{}, "", fmt.Errorf("image must be a complete static JPEG or PNG")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Height > MaxStaticImagePixels || cfg.Width > MaxStaticImagePixels/cfg.Height {
		return image.Config{}, "", fmt.Errorf("image dimensions exceed 40 million pixels")
	}
	if format == "png" {
		if err := validateStaticPNG(data); err != nil {
			return image.Config{}, "", err
		}
	}
	return cfg, format, nil
}

// PNG's standard decoder ignores APNG animation chunks. Scan bounded chunk
// boundaries as well, so accepting its default image never hides animation.
func validateStaticPNG(data []byte) error {
	for offset := 8; offset < len(data); {
		if len(data)-offset < 12 {
			return fmt.Errorf("incomplete PNG chunk")
		}
		size := uint64(binary.BigEndian.Uint32(data[offset:]))
		if size > uint64(len(data)-offset-12) {
			return fmt.Errorf("incomplete PNG chunk")
		}
		kind := string(data[offset+4 : offset+8])
		switch kind {
		case "acTL", "fcTL", "fdAT":
			return fmt.Errorf("animated PNG is unsupported")
		case "IEND":
			if size == 0 && offset+12 == len(data) {
				return nil
			}
			return fmt.Errorf("invalid PNG ending")
		}
		offset += int(size) + 12
	}
	return fmt.Errorf("incomplete PNG ending")
}

// ImageJPEGThumbnail preserves the legacy thumbnail rendering, including its
// alpha handling. Static draft preparation decodes once and uses imageThumbnail.
func ImageJPEGThumbnail(data []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return imageThumbnail(src)
}

func imageThumbnail(src image.Image) ([]byte, error) {
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, fmt.Errorf("invalid image dimensions")
	}
	dstW, dstH := ScaledImageDimensions(srcW, srcH, ImageThumbnailMaxDimension)
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			dst.Set(x, y, src.At(bounds.Min.X+x*srcW/dstW, bounds.Min.Y+y*srcH/dstH))
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 75}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func ScaledImageDimensions(width, height, maxDimension int) (int, int) {
	if width <= 0 || height <= 0 {
		return 0, 0
	}
	if maxDimension <= 0 || (width <= maxDimension && height <= maxDimension) {
		return width, height
	}
	if width >= height {
		return maxDimension, max(1, height*maxDimension/width)
	}
	return max(1, width*maxDimension/height), maxDimension
}

func BuildImageMessage(up whatsmeow.UploadResponse, caption string, meta ImageMetadata) *waE2E.ImageMessage {
	return &waE2E.ImageMessage{
		URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
		MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
		FileLength: proto.Uint64(up.FileLength), Mimetype: proto.String(meta.MIME),
		Caption: proto.String(caption), Width: proto.Uint32(meta.Width), Height: proto.Uint32(meta.Height),
		JPEGThumbnail: bytes.Clone(meta.JPEGThumbnail),
	}
}
