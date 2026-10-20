// Package fakekernel is a Jupyter kernel in Go, for the plugin's tests: a test binary runs itself as
// one (Dispatch in its TestMain), binding the connection file's ports and answering as ipykernel
// does, so the kernel client and the card are tested where no Python is. Only tests import it.
//
// Its code is a word:
//
//	print   a stream "1\n"
//	error   an error, and an error reply
//	png     a display_data of a PNG, with its text/plain
//	df      an execute_result of a DataFrame's table schema, as pandas sends it
//	vl      a display_data of a Vega-Lite v5 chart
//	set     remembers x; get prints it, or errors when the kernel was restarted
//	sleep   says "sleeping\n", then waits 30s for an interrupt
//
// Any other code prints itself to stdout.
package fakekernel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-zeromq/zmq4"

	"github.com/yongjohnlee80/autodoc-jupyter/kernel"
)

// Mode is how the fake kernel is run: the first argument it is started with.
type Mode string

const (
	// Signal is interrupted by SIGINT alone.
	Signal Mode = "fake-kernel"
	// Message is interrupted by interrupt_request alone, so a test sees which way the client
	// interrupted.
	Message Mode = "fake-kernel-messages"
	// Dies exits at once, saying on stderr that ipykernel is missing.
	Dies Mode = "fake-kernel-dies"
)

// PNG is the image "png" displays.
var PNG = []byte("\x89PNG\r\n\x1a\nfake")

// DataFrame is the table schema "df" sends, as pandas 2.2 does once display.html.table_schema is on.
const DataFrame = `{"schema": {"fields": [{"name": "index", "type": "integer"}, {"name": "a", "type": "integer"}, {"name": "b", "type": "string"}], "primaryKey": ["index"], "pandas_version": "1.4.0"}, "data": [{"index": 0, "a": 1, "b": "x"}, {"index": 1, "a": 2, "b": null}]}`

// Dispatch runs the fake kernel when this process was started as one, and exits; otherwise it
// returns. A test package's TestMain calls it first.
func Dispatch() {
	if len(os.Args) != 3 {
		return
	}
	switch Mode(os.Args[1]) {
	case Signal, Message:
		os.Exit(run(os.Args[2], Mode(os.Args[1]) == Message))
	case Dies:
		fmt.Fprintln(os.Stderr, "ModuleNotFoundError: No module named 'ipykernel'")
		os.Exit(1)
	}
}

// Spec is a kernelspec that runs this test binary as the fake kernel in mode, of language lang.
func Spec(mode Mode, lang string) (kernel.Spec, error) {
	exe, err := os.Executable()
	if err != nil {
		return kernel.Spec{}, err
	}
	interrupt := ""
	if mode == Message {
		interrupt = "message"
	}
	return kernel.Spec{Name: "fake", DisplayName: "Fake", Argv: []string{exe, string(mode), "{connection_file}"},
		Language: lang, InterruptMode: interrupt}, nil
}

func run(connFile string, byMessage bool) int {
	data, err := os.ReadFile(connFile)
	if err != nil {
		return 2
	}
	var conn struct {
		Shell   int    `json:"shell_port"`
		IOPub   int    `json:"iopub_port"`
		Control int    `json:"control_port"`
		Key     string `json:"key"`
	}
	if json.Unmarshal(data, &conn) != nil {
		return 2
	}
	ctx := context.Background()
	codec := kernel.Codec{Key: []byte(conn.Key)}
	shell, control, iopub := zmq4.NewRouter(ctx), zmq4.NewRouter(ctx), zmq4.NewPub(ctx)
	for sock, port := range map[zmq4.Socket]int{shell: conn.Shell, control: conn.Control, iopub: conn.IOPub} {
		if sock.Listen(fmt.Sprintf("tcp://127.0.0.1:%d", port)) != nil {
			return 3
		}
	}
	session := kernel.NewID()
	send := func(sock zmq4.Socket, ids [][]byte, parent kernel.Header, msgType string, content any) {
		body, _ := json.Marshal(content)
		frames, _ := codec.Encode(kernel.Message{Identities: ids, Header: kernel.NewHeader(session, msgType), Parent: parent, Content: body})
		_ = sock.Send(zmq4.NewMsgFrom(frames...))
	}
	publish := func(parent kernel.Header, msgType string, content any) { send(iopub, nil, parent, msgType, content) }

	interrupts := make(chan struct{}, 1)
	interrupted := func() {
		select {
		case interrupts <- struct{}{}:
		default:
		}
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT) // caught in both modes: by messages, a SIGINT does nothing
	go func() {
		for range sigs {
			if !byMessage {
				interrupted()
			}
		}
	}()
	quit := make(chan struct{})
	go func() {
		for {
			raw, err := control.Recv()
			if err != nil {
				return
			}
			m, err := codec.Decode(raw.Frames)
			if err != nil {
				continue
			}
			switch m.Header.MsgType {
			case "interrupt_request":
				if byMessage {
					interrupted()
				}
				send(control, m.Identities, m.Header, "interrupt_reply", map[string]any{"status": "ok"})
			case "shutdown_request":
				send(control, m.Identities, m.Header, "shutdown_reply", map[string]any{"status": "ok", "restart": false})
				close(quit)
				return
			}
		}
	}()
	go func() {
		var x string
		count := 0
		for {
			raw, err := shell.Recv()
			if err != nil {
				return
			}
			m, err := codec.Decode(raw.Frames)
			if err != nil {
				continue
			}
			publish(m.Header, "status", map[string]any{"execution_state": "busy"})
			switch m.Header.MsgType {
			case "kernel_info_request":
				send(shell, m.Identities, m.Header, "kernel_info_reply", map[string]any{"status": "ok", "language_info": map[string]any{"name": "fake"}})
			case "execute_request":
				var req struct {
					Code   string `json:"code"`
					Silent bool   `json:"silent"`
				}
				_ = json.Unmarshal(m.Content, &req)
				if !req.Silent {
					count++
				}
				publish(m.Header, "execute_input", map[string]any{"code": req.Code, "execution_count": count})
				status := "ok"
				fail := func(ename, evalue string) {
					status = "error"
					publish(m.Header, "error", map[string]any{"ename": ename, "evalue": evalue, "traceback": []string{"\x1b[0;31m" + ename + "\x1b[0m: " + evalue}})
				}
				select { // an interrupt for an earlier run is not this one's
				case <-interrupts:
				default:
				}
				switch code := strings.TrimSpace(req.Code); {
				case req.Silent:
				case code == "print":
					publish(m.Header, "stream", map[string]any{"name": "stdout", "text": "1\n"})
				case code == "error":
					fail("ValueError", "bad")
				case code == "png":
					publish(m.Header, "display_data", map[string]any{"data": map[string]any{
						"image/png": base64.StdEncoding.EncodeToString(PNG), "text/plain": "<Figure>"}, "metadata": map[string]any{}})
				case code == "df":
					publish(m.Header, "execute_result", map[string]any{"data": map[string]any{
						"application/vnd.dataresource+json": json.RawMessage(DataFrame), "text/plain": "   a  b"}, "metadata": map[string]any{}, "execution_count": count})
				case code == "vl":
					publish(m.Header, "display_data", map[string]any{"data": map[string]any{
						"application/vnd.vegalite.v5+json": map[string]any{"mark": "bar"}, "text/plain": "<VegaLite>"}, "metadata": map[string]any{}})
				case code == "set":
					x = "1"
				case code == "get":
					if x == "" {
						fail("NameError", "name 'x' is not defined")
					} else {
						publish(m.Header, "stream", map[string]any{"name": "stdout", "text": x + "\n"})
					}
				case code == "sleep":
					publish(m.Header, "stream", map[string]any{"name": "stdout", "text": "sleeping\n"})
					select {
					case <-interrupts:
						fail("KeyboardInterrupt", "")
					case <-time.After(30 * time.Second):
					}
				default:
					publish(m.Header, "stream", map[string]any{"name": "stdout", "text": code + "\n"})
				}
				send(shell, m.Identities, m.Header, "execute_reply", map[string]any{"status": status, "execution_count": count})
			}
			publish(m.Header, "status", map[string]any{"execution_state": "idle"})
		}
	}()
	<-quit
	time.Sleep(50 * time.Millisecond) // the shutdown reply leaves before the sockets close
	return 0
}
