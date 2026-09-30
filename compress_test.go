// Codec selection, pools and level mapping.

package columns

import (
	"bytes"
	"compress/flate"
	"runtime"
	"testing"
)

// TestDecompressBlock covers two checks a well-formed file never trips: a block
// that runs past the column's buffer, and one that does not decompress to its
// recorded length. Both must fail rather than leave the decoder reading part of
// a column.
func TestDecompressBlock(t *testing.T) {
	raw := make([]byte, 8)

	past := ColumnBlock{RawLength: 4, Data: []byte{1, 2, 3, 4}}
	if err := decompressBlock(raw, 6, past, CompressNone); err == nil {
		t.Error("decompressBlock past the buffer: want error, got nil")
	}
	short := ColumnBlock{RawLength: 4, Data: []byte{1, 2}}
	if err := decompressBlock(raw, 0, short, CompressNone); err == nil {
		t.Error("decompressBlock of a block shorter than its raw length: want error, got nil")
	}
	long := ColumnBlock{RawLength: 2, Data: []byte{1, 2, 3, 4}}
	if err := decompressBlock(raw, 0, long, CompressNone); err == nil {
		t.Error("decompressBlock of a block longer than its raw length: want error, got nil")
	}

	fits := ColumnBlock{RawLength: 4, Data: []byte{1, 2, 3, 4}}
	if err := decompressBlock(raw, 4, fits, CompressNone); err != nil {
		t.Errorf("decompressBlock of a fitting block: %v", err)
	}
}

// TestDecompressBlockTruncated reaches the read failure inside readAllInto. A
// stream cut mid-decompress fails differently from one that ends cleanly short
// of the space set aside for it, so the bounds and length checks in
// decompressBlock do not cover it.
func TestDecompressBlockTruncated(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 2000)
	compressed, err := Compress(data, CompressFlate)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	// Cutting the stream in half takes the terminating block with it, so the
	// reader fails rather than reporting an early end.
	cut := compressed[:len(compressed)/2]

	region := make([]byte, len(data))
	blk := ColumnBlock{RawLength: uint32(len(data)), Data: cut}
	if err := decompressBlock(region, 0, blk, CompressFlate); err == nil {
		t.Error("decompressBlock of a truncated stream: want error, got nil")
	}
}

func TestCompressCodecs(t *testing.T) {
	data := []byte{1, 2, 3}

	if got, err := Compress(data, CompressNone); err != nil || !bytes.Equal(got, data) {
		t.Errorf("Compress(None) = %v, %v, want %v, nil", got, err, data)
	}
	if got, err := Decompress(data, CompressNone); err != nil || !bytes.Equal(got, data) {
		t.Errorf("Decompress(None) = %v, %v, want %v, nil", got, err, data)
	}

	// Every stdlib codec that can read and write must round trip, and must
	// change the bytes: a codec that returned its input would hide a broken
	// implementation.
	for _, codec := range []uint8{CompressFlate, CompressGzip, CompressZlib, CompressLzw} {
		compressed, err := Compress(data, codec)
		if err != nil {
			t.Errorf("Compress(%s): %v", codecName(codec), err)
			continue
		}
		if bytes.Equal(compressed, data) {
			t.Errorf("Compress(%s) returned its input unchanged", codecName(codec))
		}
		got, err := Decompress(compressed, codec)
		if err != nil {
			t.Errorf("Decompress(%s): %v", codecName(codec), err)
			continue
		}
		if !bytes.Equal(got, data) {
			t.Errorf("Decompress(%s) = %v, want %v", codecName(codec), got, data)
		}
		// Each codec's stream is self describing, so garbage must be rejected
		// rather than decoded into something plausible.
		if _, err := Decompress([]byte{0xDE, 0xAD, 0xBE, 0xEF}, codec); err == nil {
			t.Errorf("Decompress(%s) of garbage: want error, got nil", codecName(codec))
		}
	}

	if _, err := Compress(data, CompressZstd); err == nil {
		t.Error("Compress with CompressZstd: want error, got nil")
	}
	if _, err := Compress(data, 99); err == nil {
		t.Error("Compress with an unknown codec: want error, got nil")
	}
	if _, err := Decompress(data, CompressZstd); err == nil {
		t.Error("Decompress with CompressZstd: want error, got nil")
	}
	if _, err := Decompress(data, 99); err == nil {
		t.Error("Decompress with an unknown codec: want error, got nil")
	}
}

// TestPooledFlateReaderResetError reaches the one failure a pooled decompressor
// can report. A real flate reader never fails a reset, so the test puts one in
// the pool that does. decompressInto must return that error and keep that reader
// out of the pool, where it would fail every read after it.
//
// The reader this puts in has to be the one that comes back out. sync.Pool keeps
// a private slot per P that a Get on another P cannot see, and the tests before
// this one leave real readers behind, so the Put alone is not enough: a goroutine
// that moved between the two could draw a real reader, pass this check on the
// decompression error that bad data reports anyway, and leave the failing reader
// installed for a later test to read through. One processor and an empty pool
// make the hand off certain, and a reader that cannot reset is never put back, so
// the pool is empty when this test ends.
func TestPooledFlateReaderResetError(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)

	for flateReaders.Get() != nil {
		// Discard the readers the tests before this one left behind.
	}
	flateReaders.Put(&failingResetReader{})
	if _, err := decompressInto(nil, []byte{1, 2, 3}, CompressFlate); err == nil {
		t.Error("decompressInto with a reader that cannot reset: want error, got nil")
	}
	if got, ok := flateReaders.Get().(*failingResetReader); ok {
		t.Errorf("the reader that failed to reset went back into the pool: %v", got)
	}
}

// TestCompressStreamErrors reaches each write failure inside compressStream by
// collecting into a writer that fails. Each codec buffers and flushes on its own
// schedule, so the sweep tries every failure point: a flush during Write hits
// the Write error path, and the final flush at Close hits the Close error path.
func TestCompressStreamErrors(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)
	for _, codec := range []uint8{CompressFlate, CompressGzip, CompressZlib, CompressLzw} {
		errored := false
		for n := 0; n <= 24; n++ {
			if err := compressStream(&failAfter{n: n}, data, codec, flateLevel); err != nil {
				errored = true
			}
		}
		if !errored {
			t.Errorf("compressStream(%s): no write failure observed across the sweep", codecName(codec))
		}
	}
	if err := compressStream(&failAfter{n: 0}, data, 99, flateLevel); err == nil {
		t.Error("compressStream with an unknown codec: want error, got nil")
	}
}

// compressLevel pulls a level back into the range the codecs accept, and treats
// zero as the default, since that is what an empty Options means. Go's own
// sentinels pass through: DefaultCompression is a level the stdlib understands.
func TestCompressLevel(t *testing.T) {
	for _, tc := range []struct {
		level int
		want  int
	}{
		{0, flateLevel},
		{-3, flate.HuffmanOnly},
		{flate.HuffmanOnly, flate.HuffmanOnly},
		{-1, -1},
		{flate.BestSpeed, flate.BestSpeed},
		{flate.BestCompression, flate.BestCompression},
		{100, flate.BestCompression},
	} {
		if got := compressLevel(tc.level); got != tc.want {
			t.Errorf("compressLevel(%d) = %d, want %d", tc.level, got, tc.want)
		}
	}
}

// optimizeLevel is compressLevel for the Optimize pass rather than the write.
// Zero means the best a codec offers, since a caller running Optimize has
// already decided the data is worth the cost. Anything else stays a level a
// codec accepts.
func TestOptimizeLevel(t *testing.T) {
	for _, tc := range []struct {
		level int
		want  int
	}{
		{0, flate.BestCompression},
		{-3, flate.HuffmanOnly},
		{flate.BestSpeed, flate.BestSpeed},
		{6, 6},
		{100, flate.BestCompression},
	} {
		if got := optimizeLevel(tc.level); got != tc.want {
			t.Errorf("optimizeLevel(%d) = %d, want %d", tc.level, got, tc.want)
		}
	}
}

// DEFLATE bakes its level into the writer at construction, so a pooled writer
// only serves the level it was made for. The pools are keyed by codec and level
// both, and a writer asked for level 9 has to compress at level 9 rather than at
// whatever level filled the pool first.
func TestCodecPoolIsKeyedByLevel(t *testing.T) {
	// The pools are process global, so the test uses a codec no writer asks for
	// and drops its keys on the way out: a mock left in a pool real code reads
	// is worse than no test at all.
	t.Cleanup(func() {
		codecWriters.Delete(poolKey{codec: 99, level: 1})
		codecWriters.Delete(poolKey{codec: 99, level: 9})
	})

	newWriter := func() any { return &mockCompressor{} }
	one := codecPool(99, 1, newWriter)
	nine := codecPool(99, 9, newWriter)
	if one == nine {
		t.Error("levels 1 and 9 share a pool, so a writer would compress at the wrong level")
	}
	if got := codecPool(99, 1, newWriter); got != one {
		t.Error("codecPool did not return the same pool for a level it has seen")
	}
}
