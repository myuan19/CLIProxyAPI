package grpc

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Protobuf wire types
const (
	WireVarint  = 0
	Wire64Bit   = 1
	WireBytes   = 2
	Wire32Bit   = 5
)

// Field represents a single Protobuf field parsed from wire format.
type Field struct {
	Number   uint32
	WireType uint8
	// Raw contains the original bytes of this field (tag + value).
	Raw []byte
	// Value contains only the value bytes (after the tag).
	Value []byte
}

// ParseFields parses raw Protobuf wire-format data into individual fields
// without requiring .proto definitions (schema-less parsing).
func ParseFields(data []byte) ([]Field, error) {
	var fields []Field
	pos := 0

	for pos < len(data) {
		startPos := pos

		tag, n := decodeVarint(data[pos:])
		if n == 0 {
			return nil, fmt.Errorf("protobuf: invalid varint at offset %d", pos)
		}
		pos += n

		fieldNum := uint32(tag >> 3)
		wireType := uint8(tag & 0x7)

		valueStart := pos

		switch wireType {
		case WireVarint:
			_, vn := decodeVarint(data[pos:])
			if vn == 0 {
				return nil, fmt.Errorf("protobuf: invalid varint value at offset %d", pos)
			}
			pos += vn

		case Wire64Bit:
			if pos+8 > len(data) {
				return nil, fmt.Errorf("protobuf: truncated 64-bit value at offset %d", pos)
			}
			pos += 8

		case WireBytes:
			length, ln := decodeVarint(data[pos:])
			if ln == 0 {
				return nil, fmt.Errorf("protobuf: invalid length at offset %d", pos)
			}
			pos += ln
			if pos+int(length) > len(data) {
				return nil, fmt.Errorf("protobuf: truncated bytes field at offset %d (need %d, have %d)", pos, length, len(data)-pos)
			}
			pos += int(length)

		case Wire32Bit:
			if pos+4 > len(data) {
				return nil, fmt.Errorf("protobuf: truncated 32-bit value at offset %d", pos)
			}
			pos += 4

		default:
			return nil, fmt.Errorf("protobuf: unknown wire type %d at offset %d", wireType, pos)
		}

		fields = append(fields, Field{
			Number:   fieldNum,
			WireType: wireType,
			Raw:      data[startPos:pos],
			Value:    data[valueStart:pos],
		})
	}

	return fields, nil
}

// GetBytesValue extracts the bytes/string value from a WireBytes field.
func GetBytesValue(f Field) ([]byte, error) {
	if f.WireType != WireBytes {
		return nil, fmt.Errorf("protobuf: expected wire type 2 (bytes), got %d", f.WireType)
	}
	length, n := decodeVarint(f.Value)
	if n == 0 {
		return nil, fmt.Errorf("protobuf: invalid length prefix")
	}
	start := n
	end := start + int(length)
	if end > len(f.Value) {
		return nil, fmt.Errorf("protobuf: truncated bytes value")
	}
	return f.Value[start:end], nil
}

// GetVarintValue extracts the uint64 value from a WireVarint field.
func GetVarintValue(f Field) (uint64, error) {
	if f.WireType != WireVarint {
		return 0, fmt.Errorf("protobuf: expected wire type 0 (varint), got %d", f.WireType)
	}
	v, n := decodeVarint(f.Value)
	if n == 0 {
		return 0, fmt.Errorf("protobuf: invalid varint")
	}
	return v, nil
}

// ReplaceField rebuilds a protobuf message, replacing the value of
// the specified field number with newValue. Only WireBytes fields are supported.
func ReplaceField(data []byte, fieldNum uint32, newValue []byte) ([]byte, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return nil, err
	}

	var result []byte
	replaced := false

	for _, f := range fields {
		if f.Number == fieldNum && f.WireType == WireBytes {
			result = append(result, encodeTag(fieldNum, WireBytes)...)
			result = append(result, encodeVarint(uint64(len(newValue)))...)
			result = append(result, newValue...)
			replaced = true
		} else {
			result = append(result, f.Raw...)
		}
	}

	if !replaced {
		result = append(result, encodeTag(fieldNum, WireBytes)...)
		result = append(result, encodeVarint(uint64(len(newValue)))...)
		result = append(result, newValue...)
	}

	return result, nil
}

// RemoveField rebuilds a protobuf message without the specified field number.
func RemoveField(data []byte, fieldNum uint32) ([]byte, error) {
	fields, err := ParseFields(data)
	if err != nil {
		return nil, err
	}

	var result []byte
	for _, f := range fields {
		if f.Number != fieldNum {
			result = append(result, f.Raw...)
		}
	}
	return result, nil
}

// FindField returns the first field with the given number, or nil if not found.
func FindField(fields []Field, num uint32) *Field {
	for i := range fields {
		if fields[i].Number == num {
			return &fields[i]
		}
	}
	return nil
}

// FindAllFields returns all fields with the given number.
func FindAllFields(fields []Field, num uint32) []Field {
	var result []Field
	for _, f := range fields {
		if f.Number == num {
			result = append(result, f)
		}
	}
	return result
}

// EncodeStringField creates a complete protobuf field (tag + length + value)
// for a string/bytes field.
func EncodeStringField(fieldNum uint32, value string) []byte {
	var buf []byte
	buf = append(buf, encodeTag(fieldNum, WireBytes)...)
	buf = append(buf, encodeVarint(uint64(len(value)))...)
	buf = append(buf, []byte(value)...)
	return buf
}

// EncodeSubmessageField wraps sub-message bytes as a length-delimited field.
func EncodeSubmessageField(fieldNum uint32, data []byte) []byte {
	var buf []byte
	buf = append(buf, encodeTag(fieldNum, WireBytes)...)
	buf = append(buf, encodeVarint(uint64(len(data)))...)
	buf = append(buf, data...)
	return buf
}

// EncodeVarintField creates a complete protobuf varint field.
func EncodeVarintField(fieldNum uint32, value uint64) []byte {
	var buf []byte
	buf = append(buf, encodeTag(fieldNum, WireVarint)...)
	buf = append(buf, encodeVarint(value)...)
	return buf
}

func decodeVarint(buf []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, b := range buf {
		if i >= binary.MaxVarintLen64 {
			return 0, 0
		}
		if b < 0x80 {
			if i == binary.MaxVarintLen64-1 && b > 1 {
				return 0, 0
			}
			return x | uint64(b)<<s, i + 1
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, 0
}

func encodeVarint(v uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	return buf[:n]
}

func encodeTag(fieldNum uint32, wireType uint8) []byte {
	return encodeVarint(uint64(fieldNum)<<3 | uint64(wireType))
}

// GRPCFrame represents a gRPC length-prefixed message frame.
type GRPCFrame struct {
	Compressed bool
	Data       []byte
}

// ParseGRPCFrame parses a gRPC length-prefixed message from the given data.
// gRPC framing: [1 byte compressed flag] [4 bytes big-endian length] [payload]
func ParseGRPCFrame(data []byte) (*GRPCFrame, int, error) {
	if len(data) < 5 {
		return nil, 0, fmt.Errorf("grpc: frame too short (%d bytes)", len(data))
	}

	compressed := data[0] != 0
	length := binary.BigEndian.Uint32(data[1:5])

	if length > math.MaxInt32 {
		return nil, 0, fmt.Errorf("grpc: frame too large (%d bytes)", length)
	}

	totalLen := 5 + int(length)
	if len(data) < totalLen {
		return nil, 0, fmt.Errorf("grpc: truncated frame (need %d, have %d)", totalLen, len(data))
	}

	return &GRPCFrame{
		Compressed: compressed,
		Data:       data[5:totalLen],
	}, totalLen, nil
}

// EncodeGRPCFrame encodes a protobuf message into gRPC wire format.
func EncodeGRPCFrame(data []byte, compressed bool) []byte {
	buf := make([]byte, 5+len(data))
	if compressed {
		buf[0] = 1
	}
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(data)))
	copy(buf[5:], data)
	return buf
}
