package keine

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/lzw"
	"compress/zlib"
	"fmt"
	"io"
	"sync"
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

// flateReaders recycles a DEFLATE decompressor between blocks. The decompressor
// owns the sliding window and the huffman scratch, which is where most of a
// read's memory goes once the decoders stopped allocating; a column of a few
// hundred blocks would build and drop that many of them otherwise. Reset is
// what makes one reusable, and it discards the previous block's state
// entirely, so a reader taken from the pool is indistinguishable from a new
// one.
//
// Gzip and zlib validate their checksums when the reader closes, so a pooled
// one would have to be closed before it is reused rather than reset in place;
// the flate codec is what the writer picks, and the other two are rare enough
// that they still allocate.
var flateReaders sync.Pool

// The writer pools recycle the codec machinery on the write side. A compressor
// carries the hash table and the window, which is what a write's memory is made
// of once the encoders stopped allocating: the layout experiment compresses a
// column once per codec, and every block of every column after that, so a write
// builds and drops hundreds of these. Reset makes one reusable, and none of the
// three writes anything to its destination before the first Write call, so a
// writer created against io.Discard and reset onto the real one produces the
// same bytes a fresh one would.
//
// LZW has no Reset, so it still allocates. It never wins on real data, so the
// experiment reaches it but the writer does not.
var (
	flateWriters sync.Pool
	gzipWriters  sync.Pool
	zlibWriters  sync.Pool
)

// Compress applies codec to data. CompressNone returns data unchanged. The
// result is the caller's to keep.
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

// measureScratch is the reusable destination for a layout measurement. The
// experiment compresses a column once per codec and discards nearly every
// result the moment it has measured it, so the buffer each one landed in was
// most of a write's allocation. One stays with a column for the whole write,
// and a column is the unit of parallelism, so nothing else reaches it.
//
// compress hands back a slice of that buffer, which the next call overwrites,
// so a caller has to be finished with one result before it asks for the next.
// The two that use it only read a length and decompress into a buffer of their
// own, so neither keeps anything.
type measureScratch struct {
	buf bytes.Buffer
}

// compress is Compress into the buffer this scratch keeps. The result is
// borrowed from it, not owned.
func (ms *measureScratch) compress(data []byte, codec uint8) ([]byte, error) {
	if codec == CompressNone {
		return data, nil
	}
	ms.buf.Reset()
	if err := compressStream(&ms.buf, data, codec); err != nil {
		return nil, err
	}
	return ms.buf.Bytes(), nil
}

// compressStream writes the compressed form of data to w. Errors from Close are
// reported only when Write succeeded, since a Write error is the more useful
// one to surface. Close runs either way to flush the codec's buffers, after
// which the writer goes back to its pool; Reset clears whatever state a failed
// write left behind, so nothing about an erroring stream reaches the next one.
func compressStream(w io.Writer, data []byte, codec uint8) error {
	switch codec {
	case CompressFlate:
		fw, _ := flateWriters.Get().(*flate.Writer)
		if fw == nil {
			fw, _ = flate.NewWriter(io.Discard, flateLevel)
		}
		fw.Reset(w)
		_, err := fw.Write(data)
		if cerr := fw.Close(); err == nil {
			err = cerr
		}
		flateWriters.Put(fw)
		return err
	case CompressGzip:
		gw, _ := gzipWriters.Get().(*gzip.Writer)
		if gw == nil {
			gw, _ = gzip.NewWriterLevel(io.Discard, flateLevel)
		}
		gw.Reset(w)
		_, err := gw.Write(data)
		if cerr := gw.Close(); err == nil {
			err = cerr
		}
		gzipWriters.Put(gw)
		return err
	case CompressZlib:
		zw, _ := zlibWriters.Get().(*zlib.Writer)
		if zw == nil {
			zw, _ = zlib.NewWriterLevel(io.Discard, flateLevel)
		}
		zw.Reset(w)
		_, err := zw.Write(data)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		zlibWriters.Put(zw)
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
		r, _ := flateReaders.Get().(io.ReadCloser)
		if r == nil {
			r = flate.NewReader(bytes.NewReader(nil))
		}
		// Reset reads nothing, so it cannot fail for a decompressor of this
		// codec. Whatever reports otherwise is not a reader to hand back to the
		// next block.
		if err := r.(flate.Resetter).Reset(bytes.NewReader(data), nil); err != nil {
			return nil, err
		}
		out, err := readAllInto(dst, r)
		flateReaders.Put(r)
		return out, err
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
