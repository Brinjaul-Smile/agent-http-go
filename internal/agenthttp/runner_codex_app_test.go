package agenthttp

import (
	"strings"
	"testing"
)

func TestCodexAppServerDeltaTrackerConvertsCumulativeDeltaToSuffix(t *testing.T) {
	writer := &collectStreamWriter{}
	threadID := "thread-1"
	turnID := "turn-1"
	result := codexAppServerRun{}
	tracker := newAppServerDeltaTracker()
	sendRequest := func(int, string, any) error { return nil }
	sendResponseError := func(any, int, string) error { return nil }

	lines := []string{
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"抱歉，"}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"抱歉，查询天气需要发起网络请求"}}`,
	}
	for _, line := range lines {
		if err := handleCodexAppServerLine(line, writer, sendRequest, sendResponseError, "", "", "", CodexAppServerOptions{}, &threadID, &turnID, tracker, &result); err != nil {
			t.Fatal(err)
		}
	}

	if got := strings.Join(writer.deltas, ""); got != "抱歉，查询天气需要发起网络请求" {
		t.Fatalf("deltas = %#v, joined = %q", writer.deltas, got)
	}
	if len(writer.deltas) != 2 {
		t.Fatalf("deltas = %#v, want original prefix plus suffix only", writer.deltas)
	}
	if writer.deltas[1] != "查询天气需要发起网络请求" {
		t.Fatalf("second delta = %q, want suffix only", writer.deltas[1])
	}
}

func TestCodexAppServerRespondsToUnsupportedServerRequest(t *testing.T) {
	threadID := "thread-1"
	turnID := "turn-1"
	result := codexAppServerRun{}
	tracker := newAppServerDeltaTracker()
	sendRequest := func(int, string, any) error { return nil }

	var (
		gotID      any
		gotCode    int
		gotMessage string
	)
	sendResponseError := func(id any, code int, message string) error {
		gotID = id
		gotCode = code
		gotMessage = message
		return nil
	}

	err := handleCodexAppServerLine(
		`{"id":99,"method":"currentTime/read","params":{}}`,
		nil,
		sendRequest,
		sendResponseError,
		"",
		"",
		"",
		CodexAppServerOptions{},
		&threadID,
		&turnID,
		tracker,
		&result,
	)
	if err != nil {
		t.Fatal(err)
	}

	if gotID != float64(99) {
		t.Fatalf("response id = %#v, want 99", gotID)
	}
	if gotCode != -32000 {
		t.Fatalf("error code = %d, want -32000", gotCode)
	}
	if gotMessage != "server request is not supported by agent-http-go" {
		t.Fatalf("error message = %q", gotMessage)
	}
}
