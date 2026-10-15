package ipc

import "testing"

func TestM6LogCodecLimitsAndProhibitedFields(t *testing.T) {
	if _, err := LogRecordTo("INFO", "hello", map[string]string{"request_id": "id"}); err != nil {
		t.Fatal(err)
	}
	for _, record := range []struct {
		level string
		msg   string
		attrs map[string]string
	}{
		{level: "TRACE"},
		{level: "INFO", attrs: map[string]string{"request_payload_size": "4"}},
		{level: "INFO", msg: string(make([]byte, MaxLogRecordBytes))},
	} {
		if _, err := LogRecordTo(record.level, record.msg, record.attrs); err == nil {
			t.Fatalf("accepted invalid log record: %#v", record)
		}
	}
}
