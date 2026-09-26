package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sync"

	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

var errOpenAIWSMemoryBudget = errors.New("websocket memory reservation rejected")

func IsOpenAIWSMemoryAdmissionError(err error) bool { return errors.Is(err, errOpenAIWSMemoryBudget) }

type openAIWSJSONBudgetScan struct {
	quoted, escaped bool
	depth           int
}

func (s *openAIWSJSONBudgetScan) consume(data []byte) (int64, error) {
	var nodes int64
	for _, b := range data {
		if s.quoted {
			if s.escaped {
				s.escaped = false
				continue
			}
			if b == '\\' {
				s.escaped = true
			} else if b == '"' {
				s.quoted = false
			}
			continue
		}
		switch b {
		case '"':
			s.quoted = true
		case '{', '[':
			s.depth++
			nodes++
			if s.depth > 128 {
				return 0, errOpenAIWSMemoryBudget
			}
		case '}', ']':
			s.depth--
			if s.depth < 0 {
				return 0, errOpenAIWSMemoryBudget
			}
		case ',':
			nodes++
		}
	}
	return nodes, nil
}

// Reservations bound instance-local working sets. The coefficients are
// conservative allowances, not an RSS capacity claim; deployment must calibrate.
const openAIWSMemoryMultiplier int64 = 8
const openAIWSMemoryReadChunk = 32 * 1024

// Retained items must not pin an entire frame (including unrelated metadata or
// padding) after its processing reservation has been released.
func ownedOpenAIWSReplayItems(items []json.RawMessage) []json.RawMessage {
	owned := make([]json.RawMessage, len(items))
	for i, item := range items {
		owned[i] = make([]byte, len(item))
		copy(owned[i], item)
	}
	return owned
}

type openAIWSMemoryGovernor struct {
	sockets                     map[int64]int
	mu                          sync.Mutex
	limit, perAccount, reserved int64
	accounts                    map[int64]int64
}

type openAIWSMemorySession struct {
	sockets                             int
	g                                   *openAIWSMemoryGovernor
	account, reserved, active, retained int64
	pending                             []int64
	closed                              bool
}

var openAIWSMemorySessions sync.Map // *coderws.Conn -> *openAIWSMemorySession
type openAIWSMemoryContextKey struct{}

func openAIWSMemoryFromContext(ctx context.Context) *openAIWSMemorySession {
	s, _ := ctx.Value(openAIWSMemoryContextKey{}).(*openAIWSMemorySession)
	return s
}

func (s *OpenAIGatewayService) BindOpenAIWSDualLegMemory(conn *coderws.Conn) (func(), error) {
	if s == nil || s.cfg == nil || !s.cfg.Gateway.OpenAIWS.DualLegEnabled {
		return func() {}, nil
	}
	ws := s.cfg.Gateway.OpenAIWS
	if conn == nil || ws.DualLegMemoryBudgetBytes <= 0 || ws.DualLegAccountMemoryBudgetBytes <= 0 || ws.DualLegAccountMemoryBudgetBytes > ws.DualLegMemoryBudgetBytes {
		return nil, errOpenAIWSMemoryBudget
	}
	s.openaiWSMemoryOnce.Do(func() {
		s.openaiWSMemory = &openAIWSMemoryGovernor{limit: ws.DualLegMemoryBudgetBytes, perAccount: ws.DualLegAccountMemoryBudgetBytes, accounts: make(map[int64]int64)}
	})
	session := &openAIWSMemorySession{g: s.openaiWSMemory}
	if _, loaded := openAIWSMemorySessions.LoadOrStore(conn, session); loaded {
		return func() {}, nil
	}
	return func() { openAIWSMemorySessions.Delete(conn); session.close() }, nil
}

func (s *openAIWSMemorySession) growLocked(n int64) error {
	if s.closed || n < 0 || n > s.g.limit-s.g.reserved || (s.account != 0 && n > s.g.perAccount-s.g.accounts[s.account]) {
		return errOpenAIWSMemoryBudget
	}
	s.g.reserved += n
	s.reserved += n
	if s.account != 0 {
		s.g.accounts[s.account] += n
	}
	return nil
}

func (s *openAIWSMemorySession) snapshot() (int64, int64, int64) {
	if s == nil {
		return 0, 0, 0
	}
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	return s.reserved, s.g.reserved, s.g.accounts[s.account]
}

func (s *openAIWSMemorySession) releaseLocked(n int64) {
	s.reserved -= n
	s.g.reserved -= n
	if s.account != 0 {
		s.g.accounts[s.account] -= n
		if s.g.accounts[s.account] == 0 {
			delete(s.g.accounts, s.account)
		}
	}
}

func (s *openAIWSMemorySession) bindAccount(account int64) error {
	if s == nil {
		return nil
	}
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	if s.closed || account <= 0 {
		return errOpenAIWSMemoryBudget
	}
	if s.account == account {
		return nil
	}
	if s.sockets > 0 {
		return errOpenAIWSMemoryBudget
	}
	if s.reserved > s.g.perAccount-s.g.accounts[account] {
		return errOpenAIWSMemoryBudget
	}
	if s.account != 0 {
		s.g.accounts[s.account] -= s.reserved
		if s.g.accounts[s.account] == 0 {
			delete(s.g.accounts, s.account)
		}
	}
	s.account = account
	s.g.accounts[account] += s.reserved
	return nil
}

func (s *openAIWSMemorySession) growWorking(bytes int64) error {
	if s == nil {
		return nil
	}
	if bytes < 0 || bytes > math.MaxInt64/openAIWSMemoryMultiplier {
		return errOpenAIWSMemoryBudget
	}
	n := bytes * openAIWSMemoryMultiplier
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	if err := s.growLocked(n); err != nil {
		return err
	}
	s.active += n
	return nil
}

func (s *openAIWSMemorySession) beginTurn(payload []byte) error {
	if s == nil {
		return nil
	}
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	if s.closed {
		return errOpenAIWSMemoryBudget
	}
	if len(s.pending) > 0 {
		s.active += s.pending[0]
		s.pending = s.pending[1:]
		return nil
	}
	// Trusted direct service callers may have read the first frame themselves.
	var scanner openAIWSJSONBudgetScan
	nodes, err := scanner.consume(payload)
	if err != nil {
		return err
	}
	n := (int64(len(payload)) + nodes*32) * openAIWSMemoryMultiplier
	if err := s.growLocked(n); err != nil {
		return err
	}
	s.active += n
	return nil
}

func (s *openAIWSMemorySession) admitEvent(payload []byte) error {
	if s == nil {
		return nil
	}
	var scanner openAIWSJSONBudgetScan
	nodes, err := scanner.consume(payload)
	if err != nil {
		return err
	}
	bytes := nodes * 32
	event := gjson.GetBytes(payload, "type").String()
	if event == "response.output_item.done" || event == "response.completed" || event == "response.done" {
		bytes += int64(len(payload))
	}
	return s.growWorking(bytes)
}

func (s *openAIWSMemorySession) finishTurn(historyBytes int64) error {
	if s == nil {
		return nil
	}
	if historyBytes < 0 || historyBytes > math.MaxInt64/openAIWSMemoryMultiplier {
		return errOpenAIWSMemoryBudget
	}
	n := historyBytes * openAIWSMemoryMultiplier
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	if s.closed {
		return errOpenAIWSMemoryBudget
	}
	old := s.active + s.retained
	if n > old {
		if err := s.growLocked(n - old); err != nil {
			return err
		}
	} else {
		s.releaseLocked(old - n)
	}
	s.active, s.retained = 0, n
	return nil
}

func (s *openAIWSMemorySession) close() {
	s.g.mu.Lock()
	defer s.g.mu.Unlock()
	if s.closed {
		return
	}
	if s.sockets > 0 {
		s.g.sockets[s.account] -= s.sockets
		if s.g.sockets[s.account] == 0 {
			delete(s.g.sockets, s.account)
		}
		s.sockets = 0
	}
	s.releaseLocked(s.reserved)
	s.closed = true
	s.pending = nil
}

func (s *openAIWSMemorySession) reserveSocket(limit int) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	s.g.mu.Lock()
	if s.closed || s.account <= 0 || limit <= 0 || s.g.sockets[s.account] >= limit {
		s.g.mu.Unlock()
		return nil, errOpenAIWSMemoryBudget
	}
	const bytes = int64(64*1024) * openAIWSMemoryMultiplier
	if err := s.growLocked(bytes); err != nil {
		s.g.mu.Unlock()
		return nil, err
	}
	if s.g.sockets == nil {
		s.g.sockets = make(map[int64]int)
	}
	s.g.sockets[s.account]++
	s.sockets++
	s.g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.g.mu.Lock()
			defer s.g.mu.Unlock()
			if s.closed {
				return
			}
			s.sockets--
			s.g.sockets[s.account]--
			if s.g.sockets[s.account] == 0 {
				delete(s.g.sockets, s.account)
			}
			s.releaseLocked(bytes)
		})
	}, nil
}

func readOpenAIWSMemoryBounded(conn *coderws.Conn) (coderws.MessageType, []byte, error) {
	value, ok := openAIWSMemorySessions.Load(conn)
	if !ok {
		return conn.Read(context.Background())
	}
	s := value.(*openAIWSMemorySession)
	reserved := int64(openAIWSMemoryReadChunk) * openAIWSMemoryMultiplier
	s.g.mu.Lock()
	err := s.growLocked(reserved)
	s.g.mu.Unlock()
	if err != nil {
		return 0, nil, err
	}
	success := false
	defer func() {
		s.g.mu.Lock()
		defer s.g.mu.Unlock()
		if s.closed {
			return
		}
		if success {
			s.pending = append(s.pending, reserved)
		} else {
			s.releaseLocked(reserved)
		}
	}()
	kind, reader, err := conn.Reader(context.Background())
	if err != nil {
		return kind, nil, err
	}
	var chunk [openAIWSMemoryReadChunk]byte
	var payload []byte
	var scanner openAIWSJSONBudgetScan
	for {
		n, readErr := reader.Read(chunk[:])
		if n > 0 {
			nodes, scanErr := scanner.consume(chunk[:n])
			if scanErr != nil {
				return kind, nil, scanErr
			}
			delta := (int64(n) + nodes*32) * openAIWSMemoryMultiplier
			s.g.mu.Lock()
			err = s.growLocked(delta)
			s.g.mu.Unlock()
			if err != nil {
				return kind, nil, err
			}
			reserved += delta
			payload = append(payload, chunk[:n]...)
		}
		if readErr == io.EOF {
			success = true
			return kind, payload, nil
		}
		if readErr != nil {
			return kind, nil, readErr
		}
	}
}
