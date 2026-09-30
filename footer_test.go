// Footer encoding round trip.

package keine

import (
	"reflect"
	"testing"
)

func TestFooterCodec(t *testing.T) {
	f := Footer{
		Schema: []ColumnSchema{{Name: "a", Type: TypeInt32, Nullable: true}},
		RowGroups: []RowGroupMeta{{
			NumRows:    7,
			ByteOffset: 4,
			Columns: []ColMeta{{
				ByteLength: 12,
				NullCount:  1,
				MinVal:     []byte{1},
				MaxVal:     []byte{9},
				MinLen:     1,
				MaxLen:     4,
				Encoding:   EncPlain,
				Compress:   CompressNone,
			}},
		}},
	}
	got, err := decodeFooter(encodeFooter(f))
	if err != nil {
		t.Fatalf("decodeFooter: %v", err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Errorf("footer round trip = %+v, want %+v", got, f)
	}
	if _, err := decodeFooter([]byte{0xDE, 0xAD, 0xBE, 0xEF}); err == nil {
		t.Error("decodeFooter of garbage: want error, got nil")
	}
}
