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

// flateLevel is the DEFLATE level the flate, gzip and zlib codecs use when a
// caller has not asked for one. It is always valid for the three constructors
// below.
//
// Level 3, not the default 6. On synthetic data the levels above it buy two
// tenths of a percent of size for four times the time. Level 1 is worse still:
// it writes a smaller file that costs more to read than the bytes it saved.
//
// Real data tells a different story. On a 255000-row hacker-news shard, level 9
// writes 147.43 bytes per row against level 3's 160.23, eight percent smaller,
// while both read in about 160ms because decompression does not search. Optimize
// writes at that level, and a caller who wants it without the pass can ask for it
// directly.
const flateLevel = 3

// LZW is configured least significant bit first with eight bit literals.
const (
	lzwOrder = lzw.LSB
	lzwWidth = 8
)

// flateReaders recycles a DEFLATE decompressor between blocks. The decompressor
// owns the sliding window and the huffman scratch, which is most of a read's
// memory now that the decoders no longer allocate. Without the pool, a column of
// a few hundred blocks would build and drop that many of them. Reset discards the
// previous block's state entirely, so a pooled reader is indistinguishable from
// a new one.
//
// Gzip and zlib check their checksums when the reader closes, so a pooled one
// would have to be closed rather than reset in place. The flate codec is what the
// writer picks, and the other two are rare enough that they still allocate.
var flateReaders sync.Pool

// compressLevel maps a requested level onto a valid one. The zero value means
// flateLevel and out of range values clamp to the nearest end. Go's own
// sentinels are inside that range and pass through, so a caller can ask for
// DefaultCompression or HuffmanOnly.
func compressLevel(level int) int {
	switch {
	case level == 0:
		return flateLevel
	case level < flate.HuffmanOnly:
		return flate.HuffmanOnly
	case level > flate.BestCompression:
		return flate.BestCompression
	}
	return level
}

// optimizeLevel is the level the Optimize pass measures and writes at. Zero
// means the best a codec offers, not the best it can be asked for. A caller who
// runs the pass has decided the data is worth a full walk over, and compressing
// harder costs write time without costing read time.
func optimizeLevel(level int) int {
	if level == 0 {
		return flate.BestCompression
	}
	return compressLevel(level)
}

// codecWriters recycles one DEFLATE compressor per codec and level. The level is
// fixed when a writer is built and Reset does not change it, so a writer made
// for one level cannot serve another. Pooling them together would compress at
// whichever level was asked for first and say nothing about it. A pool built
// twice is harmless, since only the first one stored is ever drawn from.
var codecWriters sync.Map

// poolKey is the codec and level a pool's writers were built for.
type poolKey struct {
	codec uint8
	level int
}

// codecPool returns the pool of writers for one codec at one level. It builds
// the pool with make when the pair has not been used before.
func codecPool(codec uint8, level int, make func() any) *sync.Pool {
	key := poolKey{codec: codec, level: level}
	if p, ok := codecWriters.Load(key); ok {
		return p.(*sync.Pool)
	}
	actual, _ := codecWriters.LoadOrStore(key, &sync.Pool{New: make})
	return actual.(*sync.Pool)
}

// Compress applies codec to data at the default level. CompressNone returns data
// unchanged. The result is the caller's to keep.
func Compress(data []byte, codec uint8) ([]byte, error) {
	return compressAt(data, codec, flateLevel)
}

// compressAt is Compress at level, which the codecs that take one are built at
// and the rest ignore. A block has to be compressed at the level its layout was
// measured at, or the file gets a different layout than the one that was chosen.
func compressAt(data []byte, codec uint8, level int) ([]byte, error) {
	level = compressLevel(level)
	switch codec {
	case CompressNone:
		return data, nil
	case CompressFlate, CompressGzip, CompressZlib, CompressLzw:
		var buf bytes.Buffer
		// A bytes.Buffer never rejects a write.
		compressStream(&buf, data, codec, level)
		return buf.Bytes(), nil
	case CompressZstd:
		return nil, fmt.Errorf("keine: zstd compression is not available in this build")
	default:
		return nil, fmt.Errorf("keine: unknown compression codec %d", codec)
	}
}

// measureScratch is the reusable destination for a layout measurement. Optimize
// compresses a column once per codec and discards nearly every result the moment
// it has measured it, so the buffer each one landed in was most of its
// allocation. One stays with a column for the whole pass, and a column is the
// unit of parallelism, so nothing else reaches it.
//
// compress hands back a slice of that buffer, which the next call overwrites, so
// a caller has to be finished with one result before it asks for the next. The
// two callers only read a length or decompress into a buffer of their own, so
// neither keeps anything.
type measureScratch struct {
	buf bytes.Buffer
}

// compress is Compress into the buffer this scratch keeps. The result is
// borrowed from it, not owned.
func (ms *measureScratch) compress(data []byte, codec uint8, level int) ([]byte, error) {
	if codec == CompressNone {
		return data, nil
	}
	ms.buf.Reset()
	if err := compressStream(&ms.buf, data, codec, compressLevel(level)); err != nil {
		return nil, err
	}
	return ms.buf.Bytes(), nil
}

// compressStream writes the compressed form of data to w. Close runs to flush
// the codec's buffers either way, and its error is reported only when Write
// succeeded, since a Write error is the more useful one to surface. The writer
// then goes back to its pool; Reset clears whatever state a failed write left
// behind, so nothing about an erroring stream reaches the next one.
//
// level has already been through compressLevel, so the constructors below cannot
// reject it. LZW has no level of its own and no Reset either, so it is still
// built fresh per call and still allocates.
func compressStream(w io.Writer, data []byte, codec uint8, level int) error {
	switch codec {
	case CompressFlate:
		p := codecPool(CompressFlate, level, func() any {
			fw, _ := flate.NewWriter(io.Discard, level)
			return fw
		})
		fw, _ := p.Get().(*flate.Writer)
		fw.Reset(w)
		_, err := fw.Write(data)
		if cerr := fw.Close(); err == nil {
			err = cerr
		}
		p.Put(fw)
		return err
	case CompressGzip:
		p := codecPool(CompressGzip, level, func() any {
			gw, _ := gzip.NewWriterLevel(io.Discard, level)
			return gw
		})
		gw, _ := p.Get().(*gzip.Writer)
		gw.Reset(w)
		_, err := gw.Write(data)
		if cerr := gw.Close(); err == nil {
			err = cerr
		}
		p.Put(gw)
		return err
	case CompressZlib:
		p := codecPool(CompressZlib, level, func() any {
			zw, _ := zlib.NewWriterLevel(io.Discard, level)
			return zw
		})
		zw, _ := p.Get().(*zlib.Writer)
		zw.Reset(w)
		_, err := zw.Write(data)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		p.Put(zw)
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
// result, so a caller decoding many chunks can reuse one buffer. A reader's
// values never point into that buffer, so recycling it is safe.
func decompressInto(dst, data []byte, codec uint8) ([]byte, error) {
	switch codec {
	case CompressNone:
		return append(dst, data...), nil
	case CompressFlate:
		r, _ := flateReaders.Get().(io.ReadCloser)
		if r == nil {
			r = flate.NewReader(bytes.NewReader(nil))
		}
		// Reset reads nothing, so it cannot fail for a decompressor of this codec.
		// A reader that reports otherwise is not one to hand back to the next block.
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
// Growth is checked rather than assumed. Every block on the read path hands this
// a destination sized to its exact decompressed length, so a full buffer means
// the reader is finished, not that it ran out of room. Growing the moment the
// buffer filled allocated a copy of the whole column once per block and
// discarded it on the next read, which was the read path's biggest allocation.
// Reading into scratch once the destination is full tells the two apart, and
// whatever the reader still returns is placed by the append that grows.
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
