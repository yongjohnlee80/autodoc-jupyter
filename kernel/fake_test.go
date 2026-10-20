package kernel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
)

// The cells' kernel is this test binary, run as "fake-kernel <connection file>": a Jupyter kernel in
// Go that binds the connection file's ports and answers as ipykernel does, so the client is tested
// where no Python is. Its code is a word:
//
//	print   a stream "1\n"
//	error   an error, and an error reply
//	png     a display_data of a PNG, with its text/plain
//	set     remembers x; get prints it, or errors when the kernel was restarted
//	sleep   says "sleeping\n", then waits 30s for an interrupt
//
// "fake-kernel" is interrupted by SIGINT alone, "fake-kernel-messages" by interrupt_request alone,
// so a cell sees which way the client interrupted.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && (os.Args[1] == "fake-kernel" || os.Args[1] == "fake-kernel-messages") {
		os.Exit(fakeKernel(os.Args[2], os.Args[1] == "fake-kernel-messages"))
	}
	if len(os.Args) == 3 && os.Args[1] == "fake-kernel-dies" {
		fmt.Fprintln(os.Stderr, "ModuleNotFoundError: No module named 'ipykernel'")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func fakeKernel(connFile string, byMessage bool) int {
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
	sign := signer{key: []byte(conn.Key)}
	shell, control, iopub := zmq4.NewRouter(ctx), zmq4.NewRouter(ctx), zmq4.NewPub(ctx)
	for sock, port := range map[zmq4.Socket]int{shell: conn.Shell, control: conn.Control, iopub: conn.IOPub} {
		if sock.Listen(fmt.Sprintf("tcp://127.0.0.1:%d", port)) != nil {
			return 3
		}
	}
	session := newID()
	send := func(sock zmq4.Socket, ids [][]byte, parent Header, msgType string, content any) {
		body, _ := json.Marshal(content)
		frames, _ := sign.encode(Message{Identities: ids, Header: newHeader(session, msgType), Parent: parent, Content: body})
		_ = sock.Send(zmq4.NewMsgFrom(frames...))
	}
	publish := func(parent Header, msgType string, content any) { send(iopub, nil, parent, msgType, content) }

	interrupts := make(chan struct{}, 1)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT) // caught in both modes: by messages, a SIGINT does nothing
	go func() {
		for range sigs {
			if byMessage {
				continue
			}
			select {
			case interrupts <- struct{}{}:
			default:
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
			m, err := sign.decode(raw.Frames)
			if err != nil {
				continue
			}
			switch m.Header.MsgType {
			case "interrupt_request":
				if byMessage {
					select {
					case interrupts <- struct{}{}:
					default:
					}
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
			m, err := sign.decode(raw.Frames)
			if err != nil {
				continue
			}
			publish(m.Header, "status", map[string]any{"execution_state": "busy"})
			switch m.Header.MsgType {
			case "kernel_info_request":
				send(shell, m.Identities, m.Header, "kernel_info_reply", map[string]any{"status": "ok", "language_info": map[string]any{"name": "fake"}})
			case "execute_request":
				var req struct {
					Code string `json:"code"`
				}
				_ = json.Unmarshal(m.Content, &req)
				count++
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
				switch strings.TrimSpace(req.Code) {
				case "print":
					publish(m.Header, "stream", map[string]any{"name": "stdout", "text": "1\n"})
				case "error":
					fail("ValueError", "bad")
				case "png":
					publish(m.Header, "display_data", map[string]any{"data": map[string]any{
						"image/png": base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake")), "text/plain": "<Figure>"}, "metadata": map[string]any{}})
				case "set":
					x = "1"
				case "get":
					if x == "" {
						fail("NameError", "name 'x' is not defined")
					} else {
						publish(m.Header, "stream", map[string]any{"name": "stdout", "text": x + "\n"})
					}
				case "sleep":
					publish(m.Header, "stream", map[string]any{"name": "stdout", "text": "sleeping\n"})
					select {
					case <-interrupts:
						fail("KeyboardInterrupt", "")
					case <-time.After(30 * time.Second):
					}
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

// fakeSpec is a kernelspec that runs this test binary as the fake kernel.
func fakeSpec(t *testing.T, mode, interruptMode string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{Name: "fake", Argv: []string{exe, mode, "{connection_file}"}, Language: "fake", InterruptMode: interruptMode}
}

// startFake starts the fake kernel, shut down when the test ends: interrupted by interrupt_request
// when interruptMode is "message", else by SIGINT.
func startFake(t *testing.T, interruptMode string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mode := "fake-kernel"
	if interruptMode == "message" {
		mode = "fake-kernel-messages"
	}
	c, err := Start(ctx, fakeSpec(t, mode, interruptMode), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	return c
}

// run executes code on c, and returns its outputs' types and texts with the reply.
func run(t *testing.T, c *Client, code string) ([]Message, Reply) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []Message
	r, err := c.Execute(ctx, code, false, func(m Message) { out = append(out, m) })
	if err != nil {
		t.Fatalf("%s: %v", code, err)
	}
	return out, r
}
