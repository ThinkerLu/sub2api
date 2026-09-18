package service

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestReadOpenAIWSClientMessage_ControlCloseFrames(t *testing.T) {
	tests := []struct {
		name          string
		timeout       time.Duration
		timeoutStatus coderws.StatusCode
		timeoutReason string
		cancelCause   error
		wantStatus    coderws.StatusCode
		wantReason    string
	}{
		{
			name:          "inter-turn idle sends normal close",
			timeout:       25 * time.Millisecond,
			timeoutStatus: coderws.StatusNormalClosure,
			timeoutReason: "websocket idle timeout",
			wantStatus:    coderws.StatusNormalClosure,
			wantReason:    "websocket idle timeout",
		},
		{
			name:          "first message timeout sends policy close",
			timeout:       25 * time.Millisecond,
			timeoutStatus: coderws.StatusPolicyViolation,
			timeoutReason: "missing first response.create message",
			wantStatus:    coderws.StatusPolicyViolation,
			wantReason:    "missing first response.create message",
		},
		{
			name:        "lease loss sends retry close",
			cancelCause: ErrOpenAIWSIngressLeaseLost,
			wantStatus:  coderws.StatusTryAgainLater,
			wantReason:  "websocket ingress capacity lease lost; please reconnect",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controlCtx, cancelControl := context.WithCancelCause(context.Background())
			defer cancelControl(context.Canceled)
			serverResult := make(chan error, 1)
			readStarted := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					serverResult <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				close(readStarted)
				_, _, err = ReadOpenAIWSClientMessage(
					controlCtx,
					conn,
					tt.timeout,
					tt.timeoutStatus,
					tt.timeoutReason,
				)
				serverResult <- err
			}))
			defer server.Close()

			dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
			clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			cancelDial()
			require.NoError(t, err)
			defer func() { _ = clientConn.CloseNow() }()
			<-readStarted
			if tt.cancelCause != nil {
				cancelControl(tt.cancelCause)
			}

			readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
			_, _, err = clientConn.Read(readCtx)
			cancelRead()
			var clientClose coderws.CloseError
			require.ErrorAs(t, err, &clientClose)
			require.Equal(t, tt.wantStatus, clientClose.Code)
			require.Equal(t, tt.wantReason, clientClose.Reason)

			select {
			case serverErr := <-serverResult:
				var closeErr *OpenAIWSClientCloseError
				require.ErrorAs(t, serverErr, &closeErr)
				require.Equal(t, tt.wantStatus, closeErr.StatusCode())
				require.Equal(t, tt.wantReason, closeErr.Reason())
			case <-time.After(time.Second):
				t.Fatal("server read goroutine did not exit after close handshake")
			}
		})
	}
}

func TestReadOpenAIWSClientMessage_ParentCancellationStillJoinsRead(t *testing.T) {
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	serverResult := make(chan error, 1)
	readStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		close(readStarted)
		_, _, err = ReadOpenAIWSClientMessage(controlCtx, conn, 0, 0, "")
		serverResult <- err
	}))
	defer server.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()
	<-readStarted
	cancelControl(errors.New("server shutting down"))
	readCtx, cancelRead := context.WithTimeout(context.Background(), time.Second)
	_, _, err = clientConn.Read(readCtx)
	cancelRead()
	var clientClose coderws.CloseError
	require.ErrorAs(t, err, &clientClose)
	require.Equal(t, coderws.StatusGoingAway, clientClose.Code)
	require.Equal(t, "websocket request canceled", clientClose.Reason)

	select {
	case <-serverResult:
	case <-time.After(time.Second):
		t.Fatal("server read goroutine leaked after parent cancellation")
	}
}

// Use a raw masked frame so progress is exercised within a single unfinished
// frame, rather than only between complete WebSocket messages.
func TestReadOpenAIWSClientFirstMessage_UploadProgress(t *testing.T) {
	for _, tt := range []struct {
		name              string
		stall, compressed bool
	}{
		{name: "slow single frame"},
		{name: "stalled single frame", stall: true},
		{name: "slow compressed frame", compressed: true},
		{name: "stalled compressed frame", stall: true, compressed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const idle = 200 * time.Millisecond
			result := make(chan openAIWSClientReadResult, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, progress, err := AcceptOpenAIWSClient(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
				if err != nil {
					result <- openAIWSClientReadResult{err: err}
					return
				}
				defer conn.CloseNow()
				kind, body, err := ReadOpenAIWSClientFirstMessage(context.Background(), conn, idle, progress)
				result <- openAIWSClientReadResult{messageType: kind, payload: body, err: err}
			}))
			defer srv.Close()
			conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(4*time.Second)))
			extension := ""
			if tt.compressed {
				extension = "Sec-WebSocket-Extensions: permessage-deflate\r\n"
			}
			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+extension+"\r\n")
			require.NoError(t, err)
			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, nil)
			require.NoError(t, err)
			require.Equal(t, 101, resp.StatusCode)
			body := bytes.Repeat([]byte("slow-body"), 100)
			wireBody := body
			opcode := byte(0x81)
			if tt.compressed {
				require.Contains(t, resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate")
				var compressed bytes.Buffer
				writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
				require.NoError(t, err)
				_, err = writer.Write(body)
				require.NoError(t, err)
				require.NoError(t, writer.Flush())
				wireBody = bytes.Clone(compressed.Bytes()[:compressed.Len()-4])
				require.NoError(t, writer.Close())
				opcode |= 0x40
			}
			header := []byte{opcode, 0xfe, 0, 0, 0, 0, 0, 0}
			binary.BigEndian.PutUint16(header[2:4], uint16(len(wireBody)))
			if len(wireBody) < 126 {
				header = []byte{opcode, 0x80 | byte(len(wireBody)), 0, 0, 0, 0}
			}

			_, err = conn.Write(header)
			require.NoError(t, err)
			started := time.Now()
			chunks := 9
			if tt.stall {
				chunks = 2
			}
			var lastWrite time.Time
			for i := 0; i < chunks; i++ {
				_, err = conn.Write(wireBody[i*len(wireBody)/9 : (i+1)*len(wireBody)/9])
				require.NoError(t, err)
				lastWrite = time.Now()
				if i+1 < chunks {
					time.Sleep(idle / 4)
				}
			}
			if tt.stall {
				// Reading the server close then closing TCP avoids waiting out the
				// library's close handshake against an intentionally unfinished frame.
				b, err := br.ReadByte()
				require.NoError(t, err)
				require.Equal(t, byte(0x88), b)
				require.GreaterOrEqual(t, time.Since(lastWrite), idle-20*time.Millisecond)
				require.NoError(t, conn.Close())
			}
			select {
			case got := <-result:
				if tt.stall {
					var closeErr *OpenAIWSClientCloseError
					require.ErrorAs(t, got.err, &closeErr)
					require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
				} else {
					require.NoError(t, got.err)
					require.Equal(t, body, got.payload)
					require.Greater(t, time.Since(started), idle)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("first message reader did not finish")
			}
		})
	}
}

func TestReadOpenAIWSClientFirstMessage_ControlsAndLimits(t *testing.T) {
	for _, tt := range []struct {
		name                  string
		ping, cancel, limited bool
	}{
		{name: "no bytes timeout"},
		{name: "ping traffic renews then idle expires", ping: true},
		{name: "lease loss cancels active connection", cancel: true},
		{name: "read limit preserved", limited: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const idle = 150 * time.Millisecond
			controlCtx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			result := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, progress, err := AcceptOpenAIWSClient(w, r, nil)
				if err != nil {
					result <- err
					return
				}
				defer conn.CloseNow()
				if tt.limited {
					conn.SetReadLimit(64)
				}
				_, _, err = ReadOpenAIWSClientFirstMessage(controlCtx, conn, idle, progress)
				result <- err
			}))
			defer srv.Close()
			ctx, finish := context.WithTimeout(context.Background(), 3*time.Second)
			defer finish()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			closed := make(chan error, 1)
			go func() { _, _, err := client.Read(ctx); closed <- err }()
			if tt.ping {
				for i := 0; i < 6; i++ {
					require.NoError(t, client.Ping(ctx))
					time.Sleep(idle / 3)
				}
			}
			if tt.cancel {
				cancel(ErrOpenAIWSIngressLeaseLost)
			}
			if tt.limited {
				require.NoError(t, client.Write(ctx, coderws.MessageText, bytes.Repeat([]byte("x"), 128)))
			}
			select {
			case err := <-closed:
				code := coderws.StatusPolicyViolation
				if tt.cancel {
					code = coderws.StatusTryAgainLater
				}
				if tt.limited {
					code = coderws.StatusMessageTooBig
				}
				require.Equal(t, code, coderws.CloseStatus(err))
			case <-ctx.Done():
				t.Fatal("client never received close")
			}
			select {
			case err := <-result:
				require.Error(t, err)
			case <-ctx.Done():
				t.Fatal("server reader did not exit")
			}
		})
	}
}

func TestAcceptOpenAIWSClient_PreservesBufferedFirstMessage(t *testing.T) {
	result := make(chan openAIWSClientReadResult, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, progress, err := AcceptOpenAIWSClient(w, r, nil)
		if err != nil {
			result <- openAIWSClientReadResult{err: err}
			return
		}
		defer conn.CloseNow()
		kind, body, err := ReadOpenAIWSClientFirstMessage(context.Background(), conn, time.Second, progress)
		result <- openAIWSClientReadResult{messageType: kind, payload: body, err: err}
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	request := []byte("GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	// Pipeline the masked WS frame with the HTTP upgrade in the same write.
	request = append(request, 0x81, 0x82, 0, 0, 0, 0, '{', '}')
	_, err = conn.Write(request)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	select {
	case got := <-result:
		require.NoError(t, got.err)
		require.Equal(t, []byte("{}"), got.payload)
	case <-time.After(2 * time.Second):
		t.Fatal("buffered message lost during upgrade")
	}
}
