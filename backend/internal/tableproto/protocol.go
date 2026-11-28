package tableproto

import "encoding/binary"

const (
	CSV            = "text/csv"
	XLSX           = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MaxBytes       = 16 << 20
	HeaderBytes    = 4
	ConvertProfile = "table-transform"
	InspectProfile = "table"
	Batch          = 10
)

type Options struct {
	ToXLSX    bool
	Delimiter byte
	Sheet     uint16
}

func (o Options) Valid() bool {
	return (o.Delimiter == ',' || o.Delimiter == ';' || o.Delimiter == '\t') &&
		o.Sheet > 0 && (!o.ToXLSX || o.Sheet == 1)
}

func (o Options) Header() []byte {
	var mode byte = 'X'
	if o.ToXLSX {
		mode = 'C'
	}
	return binary.BigEndian.AppendUint16([]byte{mode, o.Delimiter}, o.Sheet)
}

func Parse(header [HeaderBytes]byte) (Options, bool) {
	o := Options{header[0] == 'C', header[1], binary.BigEndian.Uint16(header[2:])}
	return o, (header[0] == 'C' || header[0] == 'X') && o.Valid()
}
