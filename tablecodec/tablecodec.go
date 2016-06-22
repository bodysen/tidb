// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tablecodec

import (
	"bytes"
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/types"
)

var (
	errInvalidRecordKey   = terror.ClassXEval.New(codeInvalidRecordKey, "invalid record key")
	errInvalidColumnCount = terror.ClassXEval.New(codeInvalidColumnCount, "invalid column count")
)

var (
	tablePrefix     = []byte{'t'}
	recordPrefixSep = []byte("_r")
	indexPrefixSep  = []byte("_i")
)

const (
	idLen           = 8
	prefixLen       = 1 + idLen /*tableID*/ + 2
	recordRowKeyLen = prefixLen + idLen /*handle*/
)

// EncodeRowKey encodes the table id and record handle into a kv.Key
func EncodeRowKey(tableID int64, encodedHandle []byte) kv.Key {
	buf := make([]byte, 0, recordRowKeyLen)
	buf = appendTableRecordPrefix(buf, tableID)
	buf = append(buf, encodedHandle...)
	return buf
}

// EncodeColumnKey encodes the table id, row handle and columnID into a kv.Key
func EncodeColumnKey(tableID int64, handle int64, columnID int64) kv.Key {
	buf := make([]byte, 0, recordRowKeyLen+idLen)
	buf = appendTableRecordPrefix(buf, tableID)
	buf = codec.EncodeInt(buf, handle)
	if columnID != 0 {
		buf = codec.EncodeInt(buf, columnID)
	}
	return buf
}

// DecodeRecordKey decodes the key and gets the tableID, handle and columnID.
func DecodeRecordKey(key kv.Key) (tableID int64, handle int64, columnID int64, err error) {
	k := key
	if !key.HasPrefix(tablePrefix) {
		return 0, 0, 0, errInvalidRecordKey.Gen("invalid record key - %q", k)
	}

	key = key[len(tablePrefix):]
	key, tableID, err = codec.DecodeInt(key)
	if err != nil {
		return 0, 0, 0, errors.Trace(err)
	}

	if !key.HasPrefix(recordPrefixSep) {
		return 0, 0, 0, errInvalidRecordKey.Gen("invalid record key - %q", k)
	}

	key = key[len(recordPrefixSep):]

	key, handle, err = codec.DecodeInt(key)
	if err != nil {
		return 0, 0, 0, errors.Trace(err)
	}
	if len(key) == 0 {
		return
	}

	key, columnID, err = codec.DecodeInt(key)
	if err != nil {
		return 0, 0, 0, errors.Trace(err)
	}

	return
}

// DecodeRowKey decodes the key and gets the handle.
func DecodeRowKey(key kv.Key) (int64, error) {
	_, handle, _, err := DecodeRecordKey(key)
	return handle, errors.Trace(err)
}

// DecodeValues decodes a byte slice into datums with column types.
func DecodeValues(data []byte, fts []*types.FieldType, inIndex bool) ([]types.Datum, error) {
	if data == nil {
		return nil, nil
	}
	values, err := codec.Decode(data)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(values) > len(fts) {
		return nil, errInvalidColumnCount.Gen("invalid column count %d is less than value count %d", len(fts), len(values))
	}
	if inIndex {
		// We don't need to unflatten index columns for now.
		return values, nil
	}

	for i := range values {
		values[i], err = Unflatten(values[i], fts[i])
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return values, nil
}

// DecodeColumnValue decodes data to a Datum according to the column info.
func DecodeColumnValue(data []byte, ft *types.FieldType) (types.Datum, error) {
	_, d, err := codec.DecodeOne(data)
	if err != nil {
		return types.Datum{}, errors.Trace(err)
	}
	colDatum, err := Unflatten(d, ft)
	if err != nil {
		return types.Datum{}, errors.Trace(err)
	}
	return colDatum, nil
}

// Unflatten converts a raw datum to a column datum.
func Unflatten(datum types.Datum, ft *types.FieldType) (types.Datum, error) {
	if datum.IsNull() {
		return datum, nil
	}
	switch ft.Tp {
	case mysql.TypeFloat:
		datum.SetFloat32(float32(datum.GetFloat64()))
		return datum, nil
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeYear, mysql.TypeInt24,
		mysql.TypeLong, mysql.TypeLonglong, mysql.TypeDouble, mysql.TypeTinyBlob,
		mysql.TypeMediumBlob, mysql.TypeBlob, mysql.TypeLongBlob, mysql.TypeVarchar,
		mysql.TypeString:
		return datum, nil
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp:
		var t mysql.Time
		t.Type = ft.Tp
		t.Fsp = ft.Decimal
		err := t.Unmarshal(datum.GetBytes())
		if err != nil {
			return datum, errors.Trace(err)
		}
		datum.SetMysqlTime(t)
		return datum, nil
	case mysql.TypeDuration:
		dur := mysql.Duration{Duration: time.Duration(datum.GetInt64())}
		datum.SetValue(dur)
		return datum, nil
	case mysql.TypeNewDecimal:
		if datum.Kind() == types.KindMysqlDecimal {
			if ft.Decimal >= 0 {
				dec := datum.GetMysqlDecimal().Truncate(int32(ft.Decimal))
				datum.SetMysqlDecimal(dec)
			}
			return datum, nil
		}
		dec, err := mysql.ParseDecimal(datum.GetString())
		if err != nil {
			return datum, errors.Trace(err)
		}
		if ft.Decimal >= 0 {
			dec = dec.Truncate(int32(ft.Decimal))
		}
		datum.SetValue(dec)
		return datum, nil
	case mysql.TypeEnum:
		enum, err := mysql.ParseEnumValue(ft.Elems, datum.GetUint64())
		if err != nil {
			return datum, errors.Trace(err)
		}
		datum.SetValue(enum)
		return datum, nil
	case mysql.TypeSet:
		set, err := mysql.ParseSetValue(ft.Elems, datum.GetUint64())
		if err != nil {
			return datum, errors.Trace(err)
		}
		datum.SetValue(set)
		return datum, nil
	case mysql.TypeBit:
		bit := mysql.Bit{Value: datum.GetUint64(), Width: ft.Flen}
		datum.SetValue(bit)
		return datum, nil
	}
	return datum, nil
}

// EncodeIndexSeekKey encodes an index value to kv.Key.
func EncodeIndexSeekKey(tableID int64, idxID int64, encodedValue []byte) kv.Key {
	key := make([]byte, 0, prefixLen+len(encodedValue))
	key = appendTableIndexPrefix(key, tableID)
	key = codec.EncodeInt(key, idxID)
	key = append(key, encodedValue...)
	return key
}

// DecodeIndexKey decodes datums from an index key.
func DecodeIndexKey(key kv.Key) ([]types.Datum, error) {
	b := key[prefixLen+idLen:]
	return codec.Decode(b)
}

// EncodeTableIndexPrefix encodes index prefix with tableID and idxID.
func EncodeTableIndexPrefix(tableID, idxID int64) kv.Key {
	key := make([]byte, 0, prefixLen)
	key = appendTableIndexPrefix(key, tableID)
	key = codec.EncodeInt(key, idxID)
	return key
}

// EncodeTablePrefix encodes table prefix with table ID.
func EncodeTablePrefix(tableID int64) kv.Key {
	var key kv.Key
	key = append(key, tablePrefix...)
	key = codec.EncodeInt(key, tableID)
	return key
}

// Record prefix is "t[tableID]_r".
func appendTableRecordPrefix(buf []byte, tableID int64) []byte {
	buf = append(buf, tablePrefix...)
	buf = codec.EncodeInt(buf, tableID)
	buf = append(buf, recordPrefixSep...)
	return buf
}

// Index prefix is "t[tableID]_i".
func appendTableIndexPrefix(buf []byte, tableID int64) []byte {
	buf = append(buf, tablePrefix...)
	buf = codec.EncodeInt(buf, tableID)
	buf = append(buf, indexPrefixSep...)
	return buf
}

// GenTableRecordPrefix composes record prefix with tableID: "t[tableID]_r".
func GenTableRecordPrefix(tableID int64) kv.Key {
	buf := make([]byte, 0, len(tablePrefix)+8+len(recordPrefixSep))
	return appendTableRecordPrefix(buf, tableID)
}

// GenTableIndexPrefix composes index prefix with tableID: "t[tableID]_i".
func GenTableIndexPrefix(tableID int64) kv.Key {
	buf := make([]byte, 0, len(tablePrefix)+8+len(indexPrefixSep))
	return appendTableIndexPrefix(buf, tableID)
}

// TruncateToRowKeyLen truncates the key to row key length if the key is longer than row key.
func TruncateToRowKeyLen(key kv.Key) kv.Key {
	if len(key) > recordRowKeyLen {
		return key[:recordRowKeyLen]
	}
	return key
}

type keyRangeSorter struct {
	ranges []kv.KeyRange
}

func (r *keyRangeSorter) Len() int {
	return len(r.ranges)
}

func (r *keyRangeSorter) Less(i, j int) bool {
	a := r.ranges[i]
	b := r.ranges[j]
	cmp := bytes.Compare(a.StartKey, b.StartKey)
	return cmp < 0
}

func (r *keyRangeSorter) Swap(i, j int) {
	r.ranges[i], r.ranges[j] = r.ranges[j], r.ranges[i]
}

const (
	codeInvalidRecordKey   = 4
	codeInvalidColumnCount = 5
)