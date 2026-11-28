package officeproto

import "slices"

const (
	MaxInput    = 32 << 20
	MaxOutput   = 32 << 20
	MaxBatch    = 10
	HeaderBytes = 4
)

type format struct{ extension, mime string }

var formats = []format{
	{"doc", "application/msword"},
	{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
	{"xls", "application/vnd.ms-excel"},
	{"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	{"ppt", "application/vnd.ms-powerpoint"},
	{"pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
	{"odt", "application/vnd.oasis.opendocument.text"},
	{"ods", "application/vnd.oasis.opendocument.spreadsheet"},
	{"odp", "application/vnd.oasis.opendocument.presentation"},
	{"txt", "text/plain"},
}

func MIMEs() []string {
	result := make([]string, len(formats))
	for i, f := range formats {
		result[i] = f.mime
	}
	return result
}

func Extension(mime string) string {
	index := slices.IndexFunc(formats, func(f format) bool { return f.mime == mime })
	if index == -1 {
		return ""
	}
	return formats[index].extension
}
