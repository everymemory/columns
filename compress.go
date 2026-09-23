package keine

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/lzw"
	"compress/zlib"
	"fmt"
	"io"
)

// flateLevel is the DEFLATE level used by the flate, gzip and zlib codecs.
// It is always valid for the three constructors below.
const flateLevel = flate.DefaultCompression

// LZW is configured least significant bit first with eight bit literals.
const (
	lzwOrder = lzw.LSB
	lzwWidth = 8
)

// Compress applies codec to data. CompressNone returns data unchanged.
func Compress(data []byte, codec uint8) ([]byte, error) {
	switch codec {
	case CompressNone:
		return data, nil
	case CompressFlate, CompressGzip, CompressZlib, CompressLzw:
		var buf bytes.Buffer
		// A bytes.Buffer never rejects a write.
		compressStream(&buf, data, codec)
		return buf.Bytes(), nil
	case CompressZstd:
		return nil, fmt.Errorf("keine: zstd compression is not available in this build")
	default:
		return nil, fmt.Errorf("keine: unknown compression codec %d", codec)
	}
}

// compressStream writes the compressed form of data to w. Errors from Close are
// reported only when Write succeeded, since a Write error is the more useful
// one to surface. Close runs either way to release the codec's buffers.
func compressStream(w io.Writer, data []byte, codec uint8) error {
	switch codec {
	case CompressFlate:
		fw, _ := flate.NewWriter(w, flateLevel)
		_, err := fw.Write(data)
		if cerr := fw.Close(); err == nil {
			err = cerr
		}
		return err
	case CompressGzip:
		gw, _ := gzip.NewWriterLevel(w, flateLevel)
		_, err := gw.Write(data)
		if cerr := gw.Close(); err == nil {
			err = cerr
		}
		return err
	case CompressZlib:
		zw, _ := zlib.NewWriterLevel(w, flateLevel)
		_, err := zw.Write(data)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		return err
	case CompressLzw:
		lw := lzw.NewWriter(w, lzwOrder, lzwWidth)
		_, err := lw.Write(data)
		if cerr := lw.Close(); err == nil {
			err = cerr
		}
		return err
	default:
		return fmt.Errorf("keine: unknown compression codec %d", codec)
	}
}

// Decompress is the inverse of Compress.
func Decompress(data []byte, codec uint8) ([]byte, error) {
	switch codec {
	case CompressNone:
		return data, nil
	case CompressFlate:
		r := flate.NewReader(bytes.NewReader(data))
		defer r.Close()
		return io.ReadAll(r)
	case CompressGzip:
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	case CompressZlib:
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(r)
	case CompressLzw:
		r := lzw.NewReader(bytes.NewReader(data), lzwOrder, lzwWidth)
		defer r.Close()
		return io.ReadAll(r)
	case CompressZstd:
		return nil, fmt.Errorf("keine: zstd compression is not available in this build")
	default:
		return nil, fmt.Errorf("keine: unknown compression codec %d", codec)
	}
}
