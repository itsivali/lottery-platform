package kyc

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Mock is an in-memory population register for tests and the local stack.
type Mock struct {
	mu      sync.Mutex
	records map[string]record
	fail    error
	down    error
}

type record struct {
	name string
	dob  time.Time
}

// NewMock returns an empty register.
func NewMock() *Mock { return &Mock{records: map[string]record{}} }

// Register adds a citizen.
func (m *Mock) Register(id, name string, dob time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[id] = record{name: normName(name), dob: dob}
}

// FailNext makes the next Verify call return err.
func (m *Mock) FailNext(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// SetDown makes every Verify fail until SetDown(nil) is called (a sustained outage).
func (m *Mock) SetDown(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = err
}

// Name implements Provider.
func (*Mock) Name() string { return "mock" }

// Verify implements Provider.
func (m *Mock) Verify(_ context.Context, r Request) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down != nil {
		return Result{}, m.down
	}
	if m.fail != nil {
		err := m.fail
		m.fail = nil
		return Result{}, err
	}
	rec, ok := m.records[r.IDNumber]
	if !ok {
		return Result{Ref: "mock-" + r.IDNumber}, nil
	}
	return Result{
		Found: true, NameMatch: rec.name == normName(r.FullName), DOBMatch: rec.dob.Equal(r.DateOfBirth), Ref: "mock-" + r.IDNumber,
	}, nil
}

func normName(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
