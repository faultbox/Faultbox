package star

import (
	"fmt"
	"math"
	"strconv"

	"go.starlark.net/starlark"
)

// freezeMockState copies before freezing: callers can continue editing their
// own dict, but neither they nor a handler can mutate an installed snapshot.
func freezeMockState(state *starlark.Dict) (*starlark.Dict, error) {
	v, err := copyMockState(state, 0)
	if err != nil {
		return nil, err
	}
	v.Freeze()
	return v.(*starlark.Dict), nil
}

func copyMockState(v starlark.Value, depth int) (starlark.Value, error) {
	if depth > 100 {
		return nil, fmt.Errorf("state is cyclic or exceeds 100 nesting levels")
	}
	switch x := v.(type) {
	case starlark.NoneType, starlark.Bool, starlark.Int, starlark.String:
		return x, nil
	case starlark.Float:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, fmt.Errorf("state floats must be finite")
		}
		return x, nil
	case *starlark.Dict:
		out := starlark.NewDict(x.Len())
		for _, pair := range x.Items() {
			if _, ok := pair[0].(starlark.String); !ok {
				return nil, fmt.Errorf("state dict keys must be strings")
			}
			item, err := copyMockState(pair[1], depth+1)
			if err != nil {
				return nil, err
			}
			_ = out.SetKey(pair[0], item)
		}
		return out, nil
	case *starlark.List:
		items := make([]starlark.Value, x.Len())
		for i := range items {
			item, err := copyMockState(x.Index(i), depth+1)
			if err != nil {
				return nil, err
			}
			items[i] = item
		}
		return starlark.NewList(items), nil
	default:
		return nil, fmt.Errorf("state must contain only dicts, lists and JSON scalar values (got %s)", v.Type())
	}
}

func (m *MockConfig) resetState() {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.state = m.StateInit
	if m.state == nil {
		m.state = starlark.NewDict(0)
		m.state.Freeze()
	}
	m.stateRevision = 0
}

func (m *MockConfig) stateSnapshot() (*starlark.Dict, uint64) {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.state, m.stateRevision
}

func (s *ServiceDef) setMockState(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if s.rt == nil || !s.rt.inTest.Load() || thread.Local("mock_handler") != nil {
		return nil, fmt.Errorf("%s.set_state() is only available in a test body, not at load time or inside a mock handler", s.Name)
	}
	// Nested load() paths may hold different ServiceDef objects for the same
	// name. Control the registered instance that startServices actually ran,
	// just as protocol steps and fault operations do, not a declaration alias.
	active := s.rt.services[s.Name]
	if active == nil || !active.IsMock() {
		return nil, fmt.Errorf("%s.set_state(): no registered mock with that name", s.Name)
	}
	mock := active.Mock
	var state *starlark.Dict
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "state", &state); err != nil {
		return nil, err
	}
	snapshot, err := freezeMockState(state)
	if err != nil {
		return nil, fmt.Errorf("%s.set_state(): %w", s.Name, err)
	}
	data, err := marshalJSONBody(snapshot)
	if err != nil {
		return nil, fmt.Errorf("%s.set_state(): %w", s.Name, err)
	}
	mock.stateMu.Lock()
	mock.state = snapshot
	mock.stateRevision++
	revision := mock.stateRevision
	mock.stateMu.Unlock()
	// Emit outside the lock: event subscribers may inspect the mock. Revisions
	// identify the atomic replacements even when parallel test branches race.
	s.rt.events.Emit("mock.state_changed", s.Name, map[string]string{
		"revision": strconv.FormatUint(revision, 10),
		"state":    string(data),
	})
	return starlark.None, nil
}
