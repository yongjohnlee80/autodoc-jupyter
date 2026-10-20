package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-zeromq/zmq4"
)

// Client is a running kernel. Execute runs code on it, one request at a time; Interrupt stops what
// runs; Restart starts the kernel afresh, its variables lost; Shutdown stops it for good.
type Client struct {
	Spec             Spec
	connDir, workDir string

	exec sync.Mutex // one Execute at a time; Restart waits for the one running after stopping its kernel
	mu   sync.Mutex // guards s
	s    *session
}

// Reply is an execute_reply: its status ("ok", "error" or "aborted"), and for an error its name and
// value.
type Reply struct {
	Status         string `json:"status"`
	ExecutionCount int    `json:"execution_count"`
	EName          string `json:"ename"`
	EValue         string `json:"evalue"`
}

// ErrClosed is a request of a kernel that was shut down.
var ErrClosed = errors.New("kernel: shut down")

// ReadyTimeout bounds a kernel's start: its process up, and answering kernel_info on its shell while
// its iopub is subscribed.
var ReadyTimeout = 60 * time.Second

// Start starts the kernel s in the folder workDir (a lab note's: its cells' relative paths are the
// note's), with its connection file in connDir, and waits until it answers. The kernel outlives ctx:
// ctx bounds the start only.
func Start(ctx context.Context, s Spec, connDir, workDir string) (*Client, error) {
	c := &Client{Spec: s, connDir: connDir, workDir: workDir}
	sess, err := startSession(ctx, s, connDir, workDir)
	if err != nil {
		return nil, err
	}
	c.s = sess
	return c, nil
}

func (c *Client) cur() *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.s
}

// Execute runs code, calling out with each output message of the run on iopub (stream,
// display_data, execute_result, error, update_display_data, clear_output), in order, until the
// kernel is idle again; silent runs it unrecorded. ctx's end returns at once, the code still
// running: Interrupt stops it.
func (c *Client) Execute(ctx context.Context, code string, silent bool, out func(Message)) (Reply, error) {
	c.exec.Lock()
	defer c.exec.Unlock()
	s := c.cur()
	if s == nil {
		return Reply{}, ErrClosed
	}
	return s.execute(ctx, code, silent, out)
}

// Interrupt stops the code running: an interrupt_request on control for a kernel whose spec asks
// for messages, else SIGINT to its process group.
func (c *Client) Interrupt() error {
	s := c.cur()
	if s == nil {
		return ErrClosed
	}
	if c.Spec.InterruptMode == "message" {
		return s.send(s.control, "interrupt_request", nil)
	}
	return interrupt(s.cmd)
}

// Restart stops the kernel and starts it again: a new process, its variables gone. A run in
// progress returns with the kernel's exit.
func (c *Client) Restart(ctx context.Context) error {
	if s := c.cur(); s != nil {
		s.stop()
	} else {
		return ErrClosed
	}
	c.exec.Lock()
	defer c.exec.Unlock()
	sess, err := startSession(ctx, c.Spec, c.connDir, c.workDir)
	c.mu.Lock()
	c.s = sess // nil when the start failed: the kernel is down, and Restart is the way back
	c.mu.Unlock()
	return err
}

// Alive reports whether the kernel's process is running.
func (c *Client) Alive() bool {
	s := c.cur()
	if s == nil {
		return false
	}
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// Shutdown stops the kernel for good, and returns once a run in progress has.
func (c *Client) Shutdown() {
	c.mu.Lock()
	s := c.s
	c.s = nil
	c.mu.Unlock()
	if s != nil {
		s.stop()
	}
	c.exec.Lock()
	c.exec.Unlock() //nolint:staticcheck // waits for a run in progress to return
}

// session is one process of a kernel and the sockets connected to it.
type session struct {
	sign     signer
	id       string
	cmd      *exec.Cmd
	exited   chan struct{} // closed once the process has exited
	exitErr  error
	stderr   *tail
	connFile string

	ctx                   context.Context // the sockets'; cancelled by stop
	cancel                context.CancelFunc
	shell, control, iopub zmq4.Socket
	shellIn, iopubIn      chan Message
	up                    bool // the kernel answered: shutdown is asked of it before it is killed
	stopOnce              sync.Once
}

// startSession writes a connection file, starts the kernel's process and connects its shell,
// control and iopub sockets, then waits for it to answer kernel_info.
func startSession(ctx context.Context, spec Spec, connDir, workDir string) (*session, error) {
	ports, err := freePorts(5)
	if err != nil {
		return nil, err
	}
	key := newID()
	conn, err := json.Marshal(map[string]any{
		"shell_port": ports[0], "iopub_port": ports[1], "stdin_port": ports[2], "control_port": ports[3], "hb_port": ports[4],
		"ip": "127.0.0.1", "transport": "tcp", "key": key, "signature_scheme": "hmac-sha256", "kernel_name": spec.Name,
	})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(connDir, 0o700); err != nil {
		return nil, err
	}
	connFile := filepath.Join(connDir, "kernel-"+newID()[:12]+".json")
	if err := os.WriteFile(connFile, conn, 0o600); err != nil {
		return nil, err
	}
	argv := spec.argv(connFile)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	s := &session{sign: signer{key: []byte(key)}, id: newID(), cmd: cmd, exited: make(chan struct{}), stderr: &tail{max: 4096}, connFile: connFile}
	cmd.Stderr = s.stderr
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		os.Remove(connFile)
		return nil, fmt.Errorf("kernel: start %s: %w", spec.Name, err)
	}
	go func() { s.exitErr = cmd.Wait(); close(s.exited) }()

	s.ctx, s.cancel = context.WithCancel(context.Background())
	go func() { // a process that exits withdraws the dials still waiting for its ports
		select {
		case <-s.exited:
			s.cancel()
		case <-s.ctx.Done():
		}
	}()
	dial := func(sock zmq4.Socket, port int) error {
		return sock.Dial(fmt.Sprintf("tcp://127.0.0.1:%d", port))
	}
	// a dial waits for the kernel's ports until the process exits or the start's bound passes; a send
	// waits 10s at most (zmq4's default is 5 minutes)
	opts := []zmq4.Option{zmq4.WithDialerRetry(100 * time.Millisecond), zmq4.WithDialerMaxRetries(-1), zmq4.WithTimeout(10 * time.Second)}
	s.shell = zmq4.NewDealer(s.ctx, append(opts, zmq4.WithID(zmq4.SocketIdentity(s.id)))...)
	s.control = zmq4.NewDealer(s.ctx, append(opts, zmq4.WithID(zmq4.SocketIdentity(s.id+"-control")))...)
	s.iopub = zmq4.NewSub(s.ctx, opts...)
	s.shellIn, s.iopubIn = make(chan Message, 256), make(chan Message, 1024)

	ready, cancel := context.WithTimeout(ctx, ReadyTimeout)
	defer cancel()
	started := make(chan struct{})
	defer close(started) // before cancel, which runs after it: a finished start is never withdrawn
	// the start's bound, or ctx's end, withdraws the dials of a start still running
	go func() {
		<-ready.Done()
		select {
		case <-started:
		default:
			s.cancel()
		}
	}()
	err = errors.Join(dial(s.shell, ports[0]), dial(s.control, ports[3]), dial(s.iopub, ports[1]),
		s.iopub.SetOption(zmq4.OptionSubscribe, ""))
	if err == nil {
		go s.read(s.shell, s.shellIn)
		go s.read(s.iopub, s.iopubIn)
		go s.read(s.control, nil) // replies on control (shutdown, interrupt) are not waited for
		err = s.waitReady(ready)
	}
	s.up = err == nil
	if err != nil {
		err = s.why(spec, err) // before stop, whose kill would read as the kernel's own exit
		s.stop()
		return nil, err
	}
	return s, nil
}

// why is err, said with the kernel's exit and the end of its stderr when the process is gone.
func (s *session) why(spec Spec, err error) error {
	select {
	case <-s.exited:
		msg := strings.TrimSpace(s.stderr.String())
		if msg != "" {
			return fmt.Errorf("kernel: %s exited (%v): %s", spec.Name, s.exitErr, msg)
		}
		return fmt.Errorf("kernel: %s exited (%v)", spec.Name, s.exitErr)
	default:
		return fmt.Errorf("kernel: %s: %w", spec.Name, err)
	}
}

// read decodes each message sock receives into in, dropping one whose signature does not match;
// a full in drops the message (no run is waiting for it). It returns when the socket is closed.
func (s *session) read(sock zmq4.Socket, in chan Message) {
	for {
		raw, err := sock.Recv()
		if err != nil {
			return
		}
		m, err := s.sign.decode(raw.Frames)
		if err != nil || in == nil {
			continue
		}
		select {
		case in <- m:
		default:
		}
	}
}

// sendHeader sends a request of msgType with content on sock, and returns its header.
func (s *session) sendHeader(sock zmq4.Socket, msgType string, content any) (Header, error) {
	h := newHeader(s.id, msgType)
	body, err := json.Marshal(content)
	if err != nil {
		return h, err
	}
	if content == nil {
		body = []byte("{}")
	}
	frames, err := s.sign.encode(Message{Header: h, Content: body})
	if err != nil {
		return h, err
	}
	return h, sock.Send(zmq4.NewMsgFrom(frames...))
}

func (s *session) send(sock zmq4.Socket, msgType string, content any) error {
	_, err := s.sendHeader(sock, msgType, content)
	return err
}

// waitReady asks kernel_info on shell until the kernel answers it and iopub has carried a message:
// a subscription made after the kernel published would otherwise miss a run's first outputs.
func (s *session) waitReady(ctx context.Context) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var replied, published bool
	for {
		if !replied || !published {
			if err := s.send(s.shell, "kernel_info_request", nil); err != nil {
				return err
			}
		}
		for wait := true; wait; {
			select {
			case m := <-s.shellIn:
				replied = replied || m.Header.MsgType == "kernel_info_reply"
			case <-s.iopubIn:
				published = true
			case <-tick.C:
				wait = false
			case <-s.exited:
				return errors.New("exited while starting")
			case <-ctx.Done():
				return fmt.Errorf("not answering after %v: %w", ReadyTimeout, ctx.Err())
			}
			if replied && published {
				s.drain()
				return nil
			}
		}
	}
}

// drain drops what the start left waiting: kernel_info replies and their status messages.
func (s *session) drain() {
	for {
		select {
		case <-s.shellIn:
		case <-s.iopubIn:
		default:
			return
		}
	}
}

func (s *session) execute(ctx context.Context, code string, silent bool, out func(Message)) (Reply, error) {
	h, err := s.sendHeader(s.shell, "execute_request", map[string]any{
		"code": code, "silent": silent, "store_history": !silent, "user_expressions": map[string]any{},
		"allow_stdin": false, "stop_on_error": true,
	})
	if err != nil {
		return Reply{}, err
	}
	var reply *Reply
	idle := false
	for !idle || reply == nil {
		select {
		case m := <-s.iopubIn:
			if m.Parent.MsgID != h.MsgID {
				continue
			}
			switch m.Header.MsgType {
			case "status":
				var st struct {
					State string `json:"execution_state"`
				}
				if json.Unmarshal(m.Content, &st) == nil && st.State == "idle" {
					idle = true
				}
			case "execute_input":
			default:
				if out != nil {
					out(m)
				}
			}
		case m := <-s.shellIn:
			if m.Parent.MsgID == h.MsgID && m.Header.MsgType == "execute_reply" {
				var r Reply
				if err := json.Unmarshal(m.Content, &r); err != nil {
					return Reply{}, fmt.Errorf("kernel: an execute_reply: %w", err)
				}
				reply = &r
			}
		case <-s.exited:
			return Reply{}, fmt.Errorf("kernel: exited during the run (%v)", s.exitErr)
		case <-ctx.Done():
			return Reply{}, ctx.Err()
		}
	}
	return *reply, nil
}

// stop asks the kernel to shut down, kills its process group when it has not exited after a
// moment, and closes the sockets and the connection file. Once only.
func (s *session) stop() {
	s.stopOnce.Do(func() {
		select {
		case <-s.exited:
		default:
			if s.up {
				_ = s.send(s.control, "shutdown_request", map[string]any{"restart": false})
			}
			select {
			case <-s.exited:
			case <-time.After(3 * time.Second):
				kill(s.cmd)
				<-s.exited
			}
		}
		s.cancel()
		for _, sock := range []zmq4.Socket{s.shell, s.control, s.iopub} {
			if sock != nil {
				sock.Close()
			}
		}
		os.Remove(s.connFile)
	})
}

// freePorts are n TCP ports free on 127.0.0.1 now, all held until each is known, so none repeats.
func freePorts(n int) ([]int, error) {
	ports := make([]int, 0, n)
	var held []net.Listener
	defer func() {
		for _, l := range held {
			l.Close()
		}
	}()
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		held = append(held, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// tail keeps the last max bytes written to it: a kernel's stderr, to say why it exited.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
