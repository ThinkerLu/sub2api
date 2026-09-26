package service

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"

	coderws "github.com/coder/websocket"
)

var errOpenAIWSMemoryBudget = errors.New("websocket memory reservation rejected")

// Reservations bound instance-local working sets. The coefficients are
// conservative allowances, not an RSS capacity claim; deployment must calibrate.
const openAIWSMemoryMultiplier int64 = 8
const openAIWSMemoryReadChunk = 32 * 1024

type openAIWSMemoryGovernor struct {
	mu                          sync.Mutex
	limit, perAccount, reserved int64
	accounts                    map[int64]int64
}

type openAIWSMemorySession struct {
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
	n := int64(len(payload)) * openAIWSMemoryMultiplier
	if err := s.growLocked(n); err != nil {
		return err
	}
	s.active += n
	return nil
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
	s.releaseLocked(s.reserved)
	s.closed = true
	s.pending = nil
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
	for {
		n, readErr := reader.Read(chunk[:])
		if n > 0 {
			delta := int64(n) * openAIWSMemoryMultiplier
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
