package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxFrame is the longest stdio frame forwarded to the server, newline excluded.
const maxFrame = 240000

// serveMCP adapts framing only. The authenticated server owns tool discovery and execution.
func serveMCP(ctx context.Context, p profile, input io.Reader, output io.Writer) error {
	reader := bufio.NewReader(input)
	encoder := json.NewEncoder(output)
	fail := func(id json.RawMessage, code int, message string) error {
		return encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	}
	for {
		raw, oversized, err := readFrame(reader, maxFrame)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if oversized {
			// The rest of the frame was discarded; the next frame is unaffected.
			if err := fail(leadingID(raw), -32600, "MCP frame too large"); err != nil {
				return err
			}
			continue
		}
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var frame struct{ ID json.RawMessage }
		if err := json.Unmarshal(line, &frame); err != nil {
			return errors.New("invalid MCP frame")
		}
		result, err := forwardMCP(ctx, p, line)
		if err != nil {
			if len(frame.ID) == 0 {
				continue
			}
			if err := fail(frame.ID, -32000, err.Error()); err != nil {
				return err
			}
		} else if len(result) > 0 {
			if err := encoder.Encode(result); err != nil {
				return err
			}
		}
	}
}

// readFrame reads one newline-terminated frame, holding at most max bytes plus one
// read buffer. A longer frame is consumed to its end and returned as its first max bytes.
func readFrame(r *bufio.Reader, max int) (frame []byte, oversized bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if !oversized {
			frame = append(frame, chunk...)
			if len(bytes.TrimSuffix(frame, []byte("\n"))) > max {
				frame, oversized = frame[:max], true
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(frame) > 0:
			return frame, oversized, nil
		default:
			return frame, oversized, err
		}
	}
}

// leadingID returns the top-level request id if it is complete within the prefix of
// a truncated frame, and nil (JSON null) otherwise.
func leadingID(prefix []byte) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(prefix))
	if start, err := decoder.Token(); err != nil || start != json.Delim('{') {
		return nil
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil
		}
		if key == "id" {
			var id any
			if json.Unmarshal(value, &id) != nil {
				return nil
			}
			switch id.(type) {
			case string, float64:
				return value
			}
			return nil
		}
	}
	return nil
}

func forwardMCP(ctx context.Context, p profile, line []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Origin+"/mcp", bytes.NewReader(line))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("inbox connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inbox HTTP %d", resp.StatusCode)
	}
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit || !json.Valid(data) {
		return nil, errors.New("invalid or oversized inbox response")
	}
	return data, nil
}
