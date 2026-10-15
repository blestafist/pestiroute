package ipc

import (
	"fmt"
	"strings"

	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/protobuf/proto"
)

const MaxLogRecordBytes = 8 << 10

var prohibitedLogFields = []string{"payload", "body", "credential", "secret", "token", "authorization"}

func LogRecordTo(level, message string, attributes map[string]string) (*gen.LogRecord, error) {
	if level != "DEBUG" && level != "INFO" && level != "WARN" && level != "ERROR" {
		return nil, fmt.Errorf("%w: invalid log level", ErrMalformed)
	}
	for key := range attributes {
		if prohibitedLogField(key) {
			return nil, fmt.Errorf("%w: prohibited log attribute", ErrMalformed)
		}
	}
	record := &gen.LogRecord{Level: level, Message: message, Attributes: attributes}
	if proto.Size(record) > MaxLogRecordBytes {
		return nil, fmt.Errorf("%w: log record exceeds 8 KiB", ErrMalformed)
	}
	return record, nil
}

func LogRecordFrom(record *gen.LogRecord) (string, string, map[string]string, error) {
	if record == nil || proto.Size(record) > MaxLogRecordBytes {
		return "", "", nil, fmt.Errorf("%w: invalid log record size", ErrMalformed)
	}
	if _, err := LogRecordTo(record.Level, record.Message, record.Attributes); err != nil {
		return "", "", nil, err
	}
	attributes := make(map[string]string, len(record.Attributes))
	for key, value := range record.Attributes {
		attributes[key] = value
	}
	return record.Level, record.Message, attributes, nil
}

func prohibitedLogField(key string) bool {
	key = strings.ToLower(key)
	for _, forbidden := range prohibitedLogFields {
		if strings.Contains(key, forbidden) {
			return true
		}
	}
	return false
}
