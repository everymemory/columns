package keine

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// blockSize is the default most encoded bytes a block holds. Blocks are byte
// ranges of the encoded stream, so this is a byte boundary rather than a value
// boundary. It sits well above DEFLATE's 32KB window, so a block loses little
// context to seeing only part of the column, and it is small enough that one
// column still splits into several jobs: on the 200000-row comparison file, five
// columns become about twenty. Larger blocks measured slightly smaller files and
// no faster a read, since the reader runs out of columns before it runs out of
// cores. A writer whose Options.BlockSize is zero uses this.
const blockSize = 256 * 1024

// ColumnBlock is one independently compressed range of a column's encoded bytes.
type ColumnBlock struct {
	// RawLength is len(Data) after decompression. It is len(Data) itself when the
	// codec is CompressNone.
	RawLength uint32
	Data      []byte
}

// ColumnChunk is one encoded column as stored on disk.
type ColumnChunk struct {
	Encoding   uint8
	Compress   uint8
	NullBitmap []byte
	// RawLength is the column's encoded length, which is the sum of every block's
	// RawLength. A reader sizes its decompression buffer from it, so a chunk
	// decompresses into one allocation instead of growing into its answer a piece
	// at a time.
	RawLength uint32
	Blocks    []ColumnBlock
}

// chunkHeaderLen is the fixed part of a chunk header: the encoding, the codec,
// the null bitmap length, the total raw and data lengths, and the block count.
const chunkHeaderLen = 18

// parseChunk reads a chunk out of b, which must hold exactly the chunk's bytes.
// c's slices point into b, so b has to outlive c: the reader reuses its buffer
// for the next row group, and a chunk handed to another goroutine is decoded
// before that happens.
func parseChunk(b []byte, c *ColumnChunk) error {
	if len(b) < chunkHeaderLen {
		return fmt.Errorf("keine: chunk is %d bytes, too small for a header", len(b))
	}
	c.Encoding = b[0]
	c.Compress = b[1]
	nullLen := binary.LittleEndian.Uint32(b[2:6])
	c.RawLength = binary.LittleEndian.Uint32(b[6:10])
	dataLen := binary.LittleEndian.Uint32(b[10:14])
	nblocks := binary.LittleEndian.Uint32(b[14:18])
	if nblocks == 0 {
		return fmt.Errorf("keine: chunk declares no blocks")
	}

	tableLen := uint64(chunkHeaderLen) + 8*uint64(nblocks)
	if uint64(len(b)) < tableLen {
		return fmt.Errorf("keine: chunk header declares %d blocks, chunk is %d bytes", nblocks, len(b))
	}
	dataStart := tableLen + uint64(nullLen)
	if uint64(len(b)) < dataStart+uint64(dataLen) {
		return fmt.Errorf("keine: chunk header wants %d bytes of data, chunk is %d", dataLen, len(b))
	}

	c.NullBitmap = b[tableLen:dataStart]
	c.Blocks = make([]ColumnBlock, nblocks)
	off := dataStart
	rawSum := uint64(0)
	for i := range c.Blocks {
		raw := binary.LittleEndian.Uint32(b[chunkHeaderLen+8*i:])
		stored := binary.LittleEndian.Uint32(b[chunkHeaderLen+8*i+4:])
		if off+uint64(stored) > dataStart+uint64(dataLen) {
			return fmt.Errorf("keine: block %d wants %d bytes, chunk has %d left", i, stored, dataStart+uint64(dataLen)-off)
		}
		c.Blocks[i] = ColumnBlock{RawLength: raw, Data: b[off : off+uint64(stored)]}
		off += uint64(stored)
		rawSum += uint64(raw)
	}
	if rawSum != uint64(c.RawLength) {
		return fmt.Errorf("keine: block raw lengths sum to %d, header says %d", rawSum, c.RawLength)
	}
	return nil
}

// WriteChunk writes c to w as: encoding byte, compress byte, null bitmap length,
// total raw length, total data length, block count, then two uint32 per block
// holding its raw and stored lengths, then the null bitmap and the blocks
// themselves. Integers are little-endian.
func WriteChunk(w io.Writer, c ColumnChunk) error {
	var head [chunkHeaderLen]byte
	head[0] = c.Encoding
	head[1] = c.Compress
	binary.LittleEndian.PutUint32(head[2:6], uint32(len(c.NullBitmap)))
	binary.LittleEndian.PutUint32(head[6:10], c.RawLength)
	dataLen := uint32(0)
	for _, blk := range c.Blocks {
		dataLen += uint32(len(blk.Data))
	}
	binary.LittleEndian.PutUint32(head[10:14], dataLen)
	binary.LittleEndian.PutUint32(head[14:18], uint32(len(c.Blocks)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}

	lens := make([]byte, 8*len(c.Blocks))
	for i, blk := range c.Blocks {
		binary.LittleEndian.PutUint32(lens[8*i:], blk.RawLength)
		binary.LittleEndian.PutUint32(lens[8*i+4:], uint32(len(blk.Data)))
	}
	if _, err := w.Write(lens); err != nil {
		return err
	}
	if _, err := w.Write(c.NullBitmap); err != nil {
		return err
	}
	for _, blk := range c.Blocks {
		if _, err := w.Write(blk.Data); err != nil {
			return err
		}
	}
	return nil
}

// ReadChunk reads one chunk written by WriteChunk.
func ReadChunk(r io.Reader) (ColumnChunk, error) {
	var c ColumnChunk
	var head [chunkHeaderLen]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return c, fmt.Errorf("keine: cannot read chunk header: %w", err)
	}
	c.Encoding = head[0]
	c.Compress = head[1]
	nullLen := binary.LittleEndian.Uint32(head[2:6])
	c.RawLength = binary.LittleEndian.Uint32(head[6:10])
	dataLen := binary.LittleEndian.Uint32(head[10:14])
	nblocks := binary.LittleEndian.Uint32(head[14:18])
	if nblocks == 0 {
		return c, fmt.Errorf("keine: chunk declares no blocks")
	}

	lens := make([]byte, 8*int(nblocks))
	if _, err := io.ReadFull(r, lens); err != nil {
		return c, fmt.Errorf("keine: cannot read block lengths: %w", err)
	}
	c.NullBitmap = make([]byte, nullLen)
	if _, err := io.ReadFull(r, c.NullBitmap); err != nil {
		return c, fmt.Errorf("keine: cannot read null bitmap: %w", err)
	}
	c.Blocks = make([]ColumnBlock, nblocks)
	rawSum := uint32(0)
	for i := range c.Blocks {
		c.Blocks[i].RawLength = binary.LittleEndian.Uint32(lens[8*i:])
		rawSum += c.Blocks[i].RawLength
		stored := binary.LittleEndian.Uint32(lens[8*i+4:])
		c.Blocks[i].Data = make([]byte, stored)
		if _, err := io.ReadFull(r, c.Blocks[i].Data); err != nil {
			return c, fmt.Errorf("keine: cannot read block %d: %w", i, err)
		}
	}
	if rawSum != c.RawLength {
		return c, fmt.Errorf("keine: block raw lengths sum to %d, header says %d", rawSum, c.RawLength)
	}
	if dataLen != 0 {
		// Reported for the same consistency check parseChunk makes.
		stored := uint32(0)
		for _, blk := range c.Blocks {
			stored += uint32(len(blk.Data))
		}
		if stored != dataLen {
			return c, fmt.Errorf("keine: chunk header wants %d bytes of data, blocks hold %d", dataLen, stored)
		}
	}
	return c, nil
}

// splitBlocks divides encoded into blocks of at most blockSize bytes and
// compresses each with codec. Blocks are byte ranges of the stream rather than
// ranges of values, so a decoder reading the blocks concatenated back together
// sees the same stream encodeWith produced, whichever encoding that was. A
// column shorter than a block stays one block, so a small column pays one block
// header and no compression overhead for it.
//
// Compressing is where a wide column's write time goes, and one block's bytes
// have nothing to do with another's or with any other column's, so the blocks
// share one semaphore across the whole write rather than each column
// compressing its own in order. A single long column otherwise holds one core
// for its whole run while the rest of the machine waits for it, and a real
// table's text column is most of the file. sem bounds the compressors in flight
// across every column at once, so the machine fills before the column count
// does. Block boundaries come from the encoded length alone and a block's
// compressed form depends only on its own bytes, so the blocks are the same
// ones a serial split would have produced, and the file does not depend on how
// the work was scheduled.
//
// codec is one the writer has already measured, so Compress serves it. An
// uncompressed block keeps its slice of encoded rather than copying, since
// nothing in between can overwrite it before WriteChunk consumes it.
//
// blockSz is the block size the writer is configured with, and level the level
// its codecs run at, so the blocks are the ones the configuration describes
// rather than the package's defaults.
func splitBlocks(encoded []byte, codec uint8, level, blockSz int, sem chan struct{}) []ColumnBlock {
	n := (len(encoded) + blockSz - 1) / blockSz
	if n == 0 {
		n = 1
	}
	blocks := make([]ColumnBlock, n)
	var wg sync.WaitGroup
	for i := range blocks {
		end := (i + 1) * blockSz
		if end > len(encoded) {
			end = len(encoded)
		}
		part := encoded[i*blockSz : end]
		blocks[i].RawLength = uint32(len(part))
		if codec == CompressNone {
			blocks[i].Data = part
			continue
		}
		i, part := i, part
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, _ := compressAt(part, codec, level)
			blocks[i].Data = out
		}()
	}
	wg.Wait()
	return blocks
}
