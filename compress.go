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

// flateLevel is the DEFLATE level used by the flate, gzip and zlib codecs. It is
// always valid for the three constructors below.
//
// Level 3 rather than the default 6. On two million bytes of pseudo-random
// float64, level 6 compressed 3.80x in 109ms and level 3 compressed 3.75x in
// 28ms; on four million bytes of repeated email strings, 7.93x in 44ms against
// 7.88x in 25ms. The last three levels buy two tenths of a percent for four
// times the time.
//
// Level 1 is not worth it either. On the 200000-row benchmark it wrote 9.69 B/row
// in 93ms against level 3's 9.07 B/row in 107ms, but reading the larger file took
// 34ms against 27ms. The bytes it saves going out come back as read time.
const flateLevel = 3

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
	return decompressInto(nil, data, codec)
}

// decompressInto appends the decompressed form of data to dst and returns the
// result, so a caller decoding many chunks can reuse one buffer. A reader
// returns values that never point into that buffer, so recycling it is safe.
func decompressInto(dst, data []byte, codec uint8) ([]byte, error) {
	switch codec {
	case CompressNone:
		return append(dst, data...), nil
	case CompressFlate:
		r := flate.NewReader(bytes.NewReader(data))
		defer r.Close()
		return readAllInto(dst, r)
	case CompressGzip:
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return readAllInto(dst, r)
	case CompressZlib:
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return readAllInto(dst, r)
	case CompressLzw:
		r := lzw.NewReader(bytes.NewReader(data), lzwOrder, lzwWidth)
		defer r.Close()
		return readAllInto(dst, r)
	case CompressZstd:
		return nil, fmt.Errorf("keine: zstd compression is not available in this build")
	default:
		return nil, fmt.Errorf("keine: unknown compression codec %d", codec)
	}
}

// readAllInto is io.ReadAll appending into dst, which it grows as needed and
// hands back with its capacity intact.
//
// Growth is checked rather than assumed. Every block on the read path hands
// this a destination sized to its exact decompressed length, so a full buffer
// means the reader is finished, not that it ran out of room. Growing the moment
// the buffer filled allocated a copy of the whole column once per block and
// discarded it on the next read, which was the read path's single biggest
// allocation. Reading into scratch once the destination is full tells the two
// apart: nothing more to read means the result already fits, and whatever it
// does return is placed by the append that grows.
func readAllInto(dst []byte, r io.Reader) ([]byte, error) {
	var scratch [4096]byte
	for {
		if cap(dst) > len(dst) {
			n, err := r.Read(dst[len(dst):cap(dst)])
			dst = dst[:len(dst)+n]
			if err != nil {
				if err == io.EOF {
					return dst, nil
				}
				return dst, err
			}
			continue
		}
		n, err := r.Read(scratch[:])
		if n > 0 {
			dst = append(dst, scratch[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				return dst, nil
			}
			return dst, err
		}
	}
}
