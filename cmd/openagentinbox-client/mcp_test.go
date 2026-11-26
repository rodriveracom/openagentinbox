package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOversizedMCPFrameDoesNotEndTheSession(t *testing.T) {
	var sizes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		sizes = append(sizes, len(body))
		io.WriteString(w, `{"jsonrpc":"2.0","id":"ok","result":{}}`)
	}))
	defer server.Close()

	// frame builds a request of exactly size bytes.
	frame := func(head string, size int) string {
		tail := `"}`
		return head + strings.Repeat("a", size-len(head)-len(tail)) + tail
	}
	input := strings.Join([]string{
		frame(`{"jsonrpc":"2.0","id":7,"params":"`, maxFrame+1),
		frame(`{"jsonrpc":"2.0","params":"`, 3*maxFrame),
		frame(`{"jsonrpc":"2.0","id":8,"params":"`, maxFrame),
		frame(`{"jsonrpc":"2.0","id":"last","params":"`, maxFrame+1), // no newline before EOF
	}, "\n")

	var output bytes.Buffer
	if err := serveMCP(context.Background(), profile{Origin: server.URL}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var got []string
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var reply struct {
			ID    json.RawMessage
			Error *struct{ Code int }
		}
		if err := decoder.Decode(&reply); err != nil {
			t.Fatal(err)
		}
		code := ""
		if reply.Error != nil && reply.Error.Code == -32600 {
			code = " too large"
		}
		got = append(got, string(reply.ID)+code)
	}
	want := []string{"7 too large", "null too large", `"ok"`, `"last" too large`}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("replies %q, want %q", got, want)
	}
	if len(sizes) != 1 || sizes[0] != maxFrame {
		t.Fatalf("forwarded sizes %v, want one frame of %d", sizes, maxFrame)
	}
}
