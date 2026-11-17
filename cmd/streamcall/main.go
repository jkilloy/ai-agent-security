// Command streamcall is a test client: it opens a call, sends variables, then
// streams a file as audio frames and prints the call ID.
//
//	go run ./cmd/streamcall -file call.wav -vars '{"caller":"+15551234567","encoding":"pcm16","sample_rate":8000}'
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url := flag.String("url", "ws://localhost:8080/ws/call", "call WebSocket URL")
	file := flag.String("file", "", "audio file to stream (WAV header is stripped; anything else is sent raw)")
	vars := flag.String("vars", "{}", "JSON object of call variables")
	chunk := flag.Int("chunk", 320, "bytes per frame")
	interval := flag.Duration("interval", 0, "delay between frames (e.g. 20ms for real time)")
	hangup := flag.Bool("hangup", false, "disconnect without sending an end message")
	flag.Parse()

	if err := run(*url, *file, *vars, *chunk, *interval, *hangup); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(url, file, vars string, chunk int, interval time.Duration, hangup bool) error {
	var audio []byte
	if file != "" {
		var err error
		if audio, err = os.ReadFile(file); err != nil {
			return err
		}
		audio = stripWAV(audio)
	}
	if !json.Valid([]byte(vars)) {
		return fmt.Errorf("-vars is not valid JSON")
	}

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte(vars)); err != nil {
		return err
	}
	msg, err := readMsg(ctx, conn)
	if err != nil {
		return fmt.Errorf("waiting for accept: %w", err)
	}
	fmt.Println("accepted, call_id =", msg["call_id"])

	for off := 0; off < len(audio); off += chunk {
		end := min(off+chunk, len(audio))
		if err := conn.Write(ctx, websocket.MessageBinary, audio[off:end]); err != nil {
			return err
		}
		if interval > 0 {
			time.Sleep(interval)
		}
	}
	fmt.Printf("sent %d bytes\n", len(audio))

	if hangup {
		return nil
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`)); err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	msg, err = readMsg(rctx, conn)
	if err != nil {
		return fmt.Errorf("waiting for complete: %w", err)
	}
	fmt.Printf("%s, call_id = %v\n", msg["type"], msg["call_id"])
	conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

func readMsg(ctx context.Context, conn *websocket.Conn) (map[string]any, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(data, &m)
}

// stripWAV returns the contents of a WAV file's data chunk, or b unchanged if
// it isn't a WAV file.
func stripWAV(b []byte) []byte {
	if len(b) < 12 || !bytes.Equal(b[0:4], []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WAVE")) {
		return b
	}
	for p := 12; p+8 <= len(b); {
		id := string(b[p : p+4])
		size := int(binary.LittleEndian.Uint32(b[p+4 : p+8]))
		p += 8
		if id == "data" {
			return b[p:min(p+size, len(b))]
		}
		p += size + size%2
	}
	return b
}
