// Canonical conversion of values to the Go types a type tag implies. Both the
// write path, which must normalize what a caller hands it, and the read path,
// which must return values equal to the ones written, route through here.

package columns

import (
	"fmt"
	"strconv"
	"strings"
)

// canonicalColumn converts decoded values to the Go types implied by typ, so a
// round trip gives back values equal to the ones written.
func canonicalColumn(vals []any, typ uint8) ([]any, error) {
	typed, err := canonicalColumnTyped(vals, typ)
	if err != nil {
		return nil, err
	}
	return boxValues(typed), nil
}

// canonicalColumnTyped is canonicalColumn returning a slice of the Go type typ
// implies rather than []any. The encoders and the statistics pass consume it
// directly, so a column is coerced once and never boxed on the way to disk.
func canonicalColumnTyped(vals []any, typ uint8) (any, error) {
	switch typ {
	case TypeBool:
		return canonicalTyped(vals, func(v any) (bool, error) { return coerceBool(v) })
	case TypeInt8:
		return canonicalTyped(vals, func(v any) (int8, error) {
			n, err := coerceInt(v, 8)
			if err != nil {
				return 0, err
			}
			return int8(n), nil
		})
	case TypeInt16:
		return canonicalTyped(vals, func(v any) (int16, error) {
			n, err := coerceInt(v, 16)
			if err != nil {
				return 0, err
			}
			return int16(n), nil
		})
	case TypeInt32:
		return canonicalTyped(vals, func(v any) (int32, error) {
			n, err := coerceInt(v, 32)
			if err != nil {
				return 0, err
			}
			return int32(n), nil
		})
	case TypeInt64:
		return canonicalTyped(vals, func(v any) (int64, error) { return coerceInt(v, 64) })
	case TypeUint8:
		return canonicalTyped(vals, func(v any) (uint8, error) {
			n, err := coerceUint(v, 8)
			if err != nil {
				return 0, err
			}
			return uint8(n), nil
		})
	case TypeUint16:
		return canonicalTyped(vals, func(v any) (uint16, error) {
			n, err := coerceUint(v, 16)
			if err != nil {
				return 0, err
			}
			return uint16(n), nil
		})
	case TypeUint32:
		return canonicalTyped(vals, func(v any) (uint32, error) {
			n, err := coerceUint(v, 32)
			if err != nil {
				return 0, err
			}
			return uint32(n), nil
		})
	case TypeUint64:
		return canonicalTyped(vals, func(v any) (uint64, error) { return coerceUint(v, 64) })
	case TypeFloat32:
		return canonicalTyped(vals, func(v any) (float32, error) {
			f, err := coerceFloat(v, 32)
			if err != nil {
				return 0, err
			}
			return float32(f), nil
		})
	case TypeFloat64:
		return canonicalTyped(vals, func(v any) (float64, error) { return coerceFloat(v, 64) })
	case TypeString:
		return canonicalTyped(vals, func(v any) (string, error) { return coerceString(v) })
	case TypeBytes:
		return canonicalTyped(vals, func(v any) ([]byte, error) { return coerceBytes(v) })
	default:
		return nil, fmt.Errorf("columns: unknown type %d", typ)
	}
}

// canonicalTyped coerces vals elementwise through coerce and collects the
// results into a []T.
func canonicalTyped[T any](vals []any, coerce func(any) (T, error)) ([]T, error) {
	out := make([]T, len(vals))
	for i, v := range vals {
		x, err := coerce(v)
		if err != nil {
			return nil, err
		}
		out[i] = x
	}
	return out, nil
}

func coerceBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		return strconv.ParseBool(x)
	case []byte:
		return strconv.ParseBool(string(x))
	case int64:
		return x != 0, nil
	case nil:
		return false, fmt.Errorf("columns: cannot coerce nil to bool")
	}
	return false, fmt.Errorf("columns: cannot coerce %T to bool", v)
}

func coerceInt(v any, bits int) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case uint:
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return int64(x), nil
	case float32:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseInt(x, 10, bits)
	case []byte:
		return strconv.ParseInt(string(x), 10, bits)
	case nil:
		return 0, fmt.Errorf("columns: cannot coerce nil to int")
	}
	return 0, fmt.Errorf("columns: cannot coerce %T to int", v)
}

func coerceUint(v any, bits int) (uint64, error) {
	switch x := v.(type) {
	case uint64:
		return x, nil
	case uint:
		return uint64(x), nil
	case uint8:
		return uint64(x), nil
	case uint16:
		return uint64(x), nil
	case uint32:
		return uint64(x), nil
	case int64:
		return uint64(x), nil
	case int:
		return uint64(x), nil
	case int8:
		return uint64(x), nil
	case int16:
		return uint64(x), nil
	case int32:
		return uint64(x), nil
	case float32:
		return uint64(x), nil
	case float64:
		return uint64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseUint(x, 10, bits)
	case []byte:
		return strconv.ParseUint(string(x), 10, bits)
	case nil:
		return 0, fmt.Errorf("columns: cannot coerce nil to uint")
	}
	return 0, fmt.Errorf("columns: cannot coerce %T to uint", v)
}

func coerceFloat(v any, bits int) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int8:
		return float64(x), nil
	case int16:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case uint:
		return float64(x), nil
	case uint8:
		return float64(x), nil
	case uint16:
		return float64(x), nil
	case uint32:
		return float64(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		return strconv.ParseFloat(x, bits)
	case []byte:
		return strconv.ParseFloat(string(x), bits)
	case nil:
		return 0, fmt.Errorf("columns: cannot coerce nil to float")
	}
	return 0, fmt.Errorf("columns: cannot coerce %T to float", v)
}

func coerceString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case bool:
		return strconv.FormatBool(x), nil
	case nil:
		return "", fmt.Errorf("columns: cannot coerce nil to string")
	}
	return "", fmt.Errorf("columns: cannot coerce %T to string", v)
}

// coerceBytes returns the raw bytes of v. A string holding the fmt "%v" form of
// a byte slice, which is what a dictionary encoded byte column decodes to, is
// parsed back into a slice.
func coerceBytes(v any) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return append([]byte(nil), x...), nil
	case string:
		return parseBytesList(x)
	case nil:
		return nil, fmt.Errorf("columns: cannot coerce nil to bytes")
	}
	return nil, fmt.Errorf("columns: cannot coerce %T to bytes", v)
}

func parseBytesList(s string) ([]byte, error) {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("columns: cannot parse %q as a byte slice", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []byte{}, nil
	}
	parts := strings.Fields(inner)
	out := make([]byte, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 8)
		if err != nil {
			return nil, fmt.Errorf("columns: cannot parse %q as a byte slice: %w", s, err)
		}
		out[i] = byte(n)
	}
	return out, nil
}
