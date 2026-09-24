package keine

// Type tags identify the logical type of a column's values. Strings share the
// variable length encodings used for byte slices.
const (
	TypeBool    uint8 = 0x01
	TypeInt8    uint8 = 0x02
	TypeInt16   uint8 = 0x03
	TypeInt32   uint8 = 0x04
	TypeInt64   uint8 = 0x05
	TypeUint8   uint8 = 0x06
	TypeUint16  uint8 = 0x07
	TypeUint32  uint8 = 0x08
	TypeUint64  uint8 = 0x09
	TypeFloat32 uint8 = 0x0A
	TypeFloat64 uint8 = 0x0B
	TypeBytes   uint8 = 0x0C
	TypeString  uint8 = 0x0D
)

// Encoding tags identify how a column chunk's values were laid out.
const (
	EncPlain       uint8 = 0x01
	EncRLEBitpack  uint8 = 0x02
	EncDelta       uint8 = 0x03
	EncDict        uint8 = 0x04
	EncOffsetBytes uint8 = 0x05
	EncAffix       uint8 = 0x06
)

// Compress tags identify the compression codec applied to a chunk's encoded
// data. CompressZstd is unimplemented and reports an error.
const (
	CompressNone  uint8 = 0x00
	CompressZstd  uint8 = 0x01
	CompressFlate uint8 = 0x02
	CompressGzip  uint8 = 0x03
	CompressZlib  uint8 = 0x04
	CompressLzw   uint8 = 0x05
)
