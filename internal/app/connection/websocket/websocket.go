package websocket

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	websocket "github.com/fasthttp/websocket"
	"github.com/hashicorp/go-multierror"
	"github.com/seventv/api/data/events"
	"github.com/seventv/common/structures/v3"
	client "github.com/seventv/eventapi/internal/app/connection"
	"github.com/seventv/eventapi/internal/global"
	"go.uber.org/zap"
)

type WebSocket struct {
	c                 *websocket.Conn
	ctx               context.Context
	cancel            context.CancelFunc
	closed            bool
	seq               int64
	handler           client.Handler
	evm               client.EventMap
	cache             client.Cache
	dig               client.EventDigest
	writeMtx          *sync.Mutex
	ready             chan struct{}
	close             chan struct{}
	sessionID         []byte
	heartbeatInterval uint32
	heartbeatCount    uint64
	subscriptionLimit int32
}

func NewWebSocket(gctx global.Context, conn *websocket.Conn, dig client.EventDigest) (client.Connection, error) {
	hbi := gctx.Config().API.HeartbeatInterval
	if hbi == 0 {
		hbi = 45000
	}

	sessionID, err := client.GenerateSessionID(32)
	if err != nil {
		return nil, err
	}

	lctx, cancel := context.WithCancel(context.Background())
	ws := &WebSocket{
		c:                 conn,
		ctx:               lctx,
		cancel:            cancel,
		seq:               0,
		evm:               client.NewEventMap(make(chan string, 10)),
		cache:             client.NewCache(),
		dig:               dig,
		writeMtx:          &sync.Mutex{},
		ready:             make(chan struct{}, 1),
		close:             make(chan struct{}),
		sessionID:         sessionID,
		heartbeatInterval: hbi,
		heartbeatCount:    0,
		subscriptionLimit: gctx.Config().API.SubscriptionLimit,
	}

	ws.handler = client.NewHandler(ws)

	return ws, nil
}

func (w *WebSocket) Context() context.Context {
	return w.ctx
}

func (w *WebSocket) SessionID() string {
	return hex.EncodeToString(w.sessionID)
}

func (w *WebSocket) Greet() error {
	msg := events.NewMessage(events.OpcodeHello, events.HelloPayload{
		HeartbeatInterval: uint32(w.heartbeatInterval),
		SessionID:         hex.EncodeToString(w.sessionID),
		SubscriptionLimit: w.subscriptionLimit,
	})

	return w.Write(msg.ToRaw())
}

func (w *WebSocket) SendHeartbeat() error {
	w.heartbeatCount++
	msg := events.NewMessage(events.OpcodeHeartbeat, events.HeartbeatPayload{
		Count: w.heartbeatCount,
	})

	return w.Write(msg.ToRaw())
}

func (w *WebSocket) SendAck(cmd events.Opcode, data json.RawMessage) error {
	msg := events.NewMessage(events.OpcodeAck, events.AckPayload{
		Command: cmd.String(),
		Data:    data,
	})

	return w.Write(msg.ToRaw())
}

func (w *WebSocket) Close(code events.CloseCode, after time.Duration) {
	if w.closed {
		return
	}

	// Send "end of stream" message
	msg := events.NewMessage(events.OpcodeEndOfStream, events.EndOfStreamPayload{
		Code:    code,
		Message: code.String(),
	})

	if err := w.Write(msg.ToRaw()); err != nil {
		zap.S().Errorw("failed to close connection", "error", err)
	}

	// Write close frame
	err := w.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(int(code), code.String()), time.Now().Add(5*time.Second))
	err = multierror.Append(err, w.c.Close()).ErrorOrNil()
	if err != nil {
		zap.S().Errorw("failed to close connection", "error", err)
	}

	select {
	case <-w.ctx.Done():
	case <-w.close:
	case <-time.After(after):
	}

	w.cancel()
	w.closed = true
}

func (w *WebSocket) Write(msg events.Message[json.RawMessage]) error {
	if w.closed {
		return nil
	}

	w.writeMtx.Lock()
	defer w.writeMtx.Unlock()

	if err := w.c.WriteJSON(msg); err != nil {
		return err
	}

	return nil
}

func (w *WebSocket) Events() client.EventMap {
	return w.evm
}

func (w *WebSocket) Cache() client.Cache {
	return w.cache
}

func (w *WebSocket) Digest() client.EventDigest {
	return w.dig
}

func (*WebSocket) Actor() *structures.User {
	// TODO: Return the actor here when authentication is implemented
	return nil
}

func (w *WebSocket) Handler() client.Handler {
	return w.handler
}

// SendError implements Connection
func (w *WebSocket) SendError(txt string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}

	msg := events.NewMessage(events.OpcodeError, events.ErrorPayload{
		Message: txt,
		Fields:  fields,
	})

	if err := w.Write(msg.ToRaw()); err != nil {
		zap.S().Errorw("failed to write an error message to the socket", "error", err)
	}
}

func (w *WebSocket) OnReady() <-chan struct{} {
	return w.ready
}

func (w *WebSocket) OnClose() <-chan struct{} {
	return w.close
}

func (*WebSocket) SetWriter(w *bufio.Writer) {
	zap.S().Fatalw("called SetWriter() on a WebSocket connection")
}
