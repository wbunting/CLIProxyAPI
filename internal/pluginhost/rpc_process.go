package pluginhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// processRPCPluginClient runs a plugin as an independent process. Requests use
// one JSON line per call so a plugin can retain state without sharing the host
// Go toolchain or dependency graph.
type processRPCPluginClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	done   chan struct{}
}

type processRPCRequest struct {
	Method  string          `json:"method"`
	Request json.RawMessage `json:"request"`
}

func newProcessRPCPluginClient(path string) (*processRPCPluginClient, error) {
	cmd := exec.Command(path)
	// stdout is reserved for protocol frames; keep plugin diagnostics visible in
	// the host service log through stderr.
	cmd.Stderr = os.Stderr
	stdin, errStartIn := cmd.StdinPipe()
	if errStartIn != nil {
		return nil, fmt.Errorf("open plugin stdin: %w", errStartIn)
	}
	stdout, errStartOut := cmd.StdoutPipe()
	if errStartOut != nil {
		return nil, fmt.Errorf("open plugin stdout: %w", errStartOut)
	}
	if errStart := cmd.Start(); errStart != nil {
		return nil, fmt.Errorf("start plugin process: %w", errStart)
	}
	client := &processRPCPluginClient{
		cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), done: make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(client.done)
	}()
	return client, nil
}

func (c *processRPCPluginClient) Call(ctx context.Context, method string, request []byte) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("plugin process client is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return nil, fmt.Errorf("plugin process exited")
	default:
	}
	payload, errMarshal := json.Marshal(processRPCRequest{Method: method, Request: json.RawMessage(request)})
	if errMarshal != nil {
		return nil, fmt.Errorf("encode plugin process request: %w", errMarshal)
	}
	if _, errWrite := c.stdin.Write(append(payload, '\n')); errWrite != nil {
		return nil, fmt.Errorf("write plugin process request: %w", errWrite)
	}
	type lineResult struct {
		line []byte
		err  error
	}
	result := make(chan lineResult, 1)
	go func() {
		line, errRead := c.stdout.ReadBytes('\n')
		result <- lineResult{line: line, err: errRead}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case response := <-result:
		if response.err != nil {
			return nil, fmt.Errorf("read plugin process response: %w", response.err)
		}
		return bytes.TrimSpace(response.line), nil
	}
}

func (c *processRPCPluginClient) Shutdown() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	cmd := c.cmd
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	select {
	case <-c.done:
	default:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-c.done
	}
}

type hybridPluginLoader struct {
	dynamic pluginLoader
}

func (l hybridPluginLoader) Open(file pluginFile, host *Host) (pluginClient, error) {
	if file.Transport == pluginTransportProcessRPC {
		return newProcessRPCPluginClient(file.Path)
	}
	return l.dynamic.Open(file, host)
}
