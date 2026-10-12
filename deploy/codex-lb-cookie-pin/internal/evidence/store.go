// Package evidence owns the plugin's non-secret, account-scoped event journal.
// Each fsynced record contains the next state, so recovery needs only a bounded
// tail read. No credentials, request bodies, templates or cookies belong here.
package evidence

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrBlocked = errors.New("probe operation requires reconciliation")
	ErrBackoff = errors.New("account backoff active")
)

const (
	maxRecord = 16 << 10
	// Two bounded segments retain recent replay evidence. Every record is a
	// complete checkpoint; rotation never expires protection or unknown state.
	segmentBytes = 8 << 20
)

type Cohort struct {
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	EffortForm string `json:"effort_form"`
	Bucket     string `json:"egress_bucket"`
}

type Event struct {
	Cohort
	AccountID int64     `json:"account_id"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Result    string    `json:"result,omitempty"`
	Operation string    `json:"operation_id,omitempty"`
	Step      int       `json:"backoff_step"`
	Until     time.Time `json:"not_before"`
	Alert     bool      `json:"alert,omitempty"`
}

type Operation struct {
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	Phase      string `json:"phase"`
	Cohort     Cohort `json:"cohort"`
	Reconciled bool   `json:"reconciled,omitempty"`
}

type State struct {
	AccountID          int64                `json:"account_id"`
	Sequence           uint64               `json:"sequence"`
	Step               int                  `json:"backoff_step"`
	NotBefore          time.Time            `json:"not_before"`
	QualityUntil       time.Time            `json:"quality_until"`
	ConsecutiveFails   int                  `json:"consecutive_fails"`
	Operation          Operation            `json:"operation"`
	ProbePassed        uint64               `json:"probe_passed"`
	BusinessDispatched uint64               `json:"business_dispatched"`
	BusinessSucceeded  uint64               `json:"business_succeeded"`
	Alerts             map[string]time.Time `json:"alerts,omitempty"`
	LastEvent          Event                `json:"last_event"`
}

func (s State) Deadline() time.Time {
	if s.QualityUntil.After(s.NotBefore) {
		return s.QualityUntil
	}
	return s.NotBefore
}

type record struct {
	Version int             `json:"version"`
	State   json.RawMessage `json:"state"`
	Hash    string          `json:"sha256"`
}

type Store struct {
	dir   string
	owner string
	mu    sync.Mutex
	locks [64]sync.Mutex
	// Used only by explicit in-memory unit fixtures. Production Open never
	// falls back to this map when durable storage is missing or broken.
	memory map[int64]State
}

func ownerID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("cannot create evidence owner identity")
	}
	return hex.EncodeToString(id[:])
}

func NewMemory() *Store { return &Store{owner: ownerID(), memory: map[int64]State{}} }

func Open(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("plugin evidence directory must be absolute")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !privateMode(info.Mode()) {
		return nil, errors.New("plugin evidence directory must be private and real")
	}
	return &Store{dir: dir, owner: ownerID()}, nil
}

// update serializes independent plugin generations with an OS lock, not a
// stale-prone PID file. A failed append/sync never grants outbound permission.
func (s *Store) update(id int64, edit func(*State) bool) (State, error) {
	if id <= 0 {
		return State{}, errors.New("invalid evidence account")
	}
	if s.memory != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		state := s.memory[id]
		if state.AccountID == 0 {
			state.AccountID = id
		}
		if edit != nil && edit(&state) {
			state.Sequence++
			s.memory[id] = state
		}
		return cloneState(state), nil
	}
	s.locks[id%int64(len(s.locks))].Lock()
	defer s.locks[id%int64(len(s.locks))].Unlock()
	name := filepath.Join(s.dir, strconv.FormatInt(id, 10)+".jsonl")
	// Lock a stable inode, not a journal that can be rotated by another process.
	lock, err := openPrivate(name+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return State{}, err
	}
	defer lock.Close()
	if err := lockFile(lock); err != nil {
		return State{}, err
	}
	defer unlockFile(lock)
	f, err := openPrivate(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = f.Close() }()
	state, end, err := readTail(f, id)
	if err != nil {
		return State{}, err
	}
	if end == 0 {
		previous, openErr := openPrivate(name+".previous", os.O_RDONLY)
		if openErr == nil {
			state, _, err = readTail(previous, id)
			_ = previous.Close()
			if err != nil {
				return State{}, err
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			return State{}, openErr
		}
	}
	if edit == nil || !edit(&state) {
		return state, nil
	}
	state.Sequence++
	raw, err := json.Marshal(state)
	if err != nil {
		return State{}, err
	}
	hash := sha256.Sum256(raw)
	line, err := json.Marshal(record{1, raw, hex.EncodeToString(hash[:])})
	if err != nil || len(line)+1 > maxRecord {
		return State{}, errors.New("invalid evidence record size")
	}
	if end+int64(len(line)+1) > segmentBytes {
		if err := f.Close(); err != nil {
			return State{}, err
		}
		if err := os.Remove(name + ".previous"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return State{}, err
		}
		if err := os.Rename(name, name+".previous"); err != nil {
			return State{}, err
		}
		if err := syncDirectory(s.dir); err != nil {
			return State{}, err
		}
		f, err = openPrivate(name, os.O_CREATE|os.O_EXCL|os.O_RDWR)
		if err != nil {
			return State{}, err
		}
		end = 0
	}
	// Only an incomplete final record can be discarded. A malformed committed
	// record is corruption, never a reason to reset backoff or unknown state.
	if err := f.Truncate(end); err != nil {
		return State{}, err
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		return State{}, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return State{}, err
	}
	if err := f.Sync(); err != nil {
		return State{}, err
	}
	if err := syncDirectory(s.dir); err != nil {
		return State{}, err
	}
	return state, nil
}

func openPrivate(name string, flags int) (*os.File, error) {
	info, err := os.Lstat(name)
	if err == nil && (!info.Mode().IsRegular() || !privateMode(info.Mode())) {
		return nil, errors.New("unsafe evidence file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(name, flags, 0600)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	current, statErr := os.Lstat(name)
	if err != nil || statErr != nil || !opened.Mode().IsRegular() ||
		!privateMode(opened.Mode()) || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, current) || (info != nil && !os.SameFile(info, opened)) {
		_ = f.Close()
		return nil, errors.New("evidence file changed during open")
	}
	return f, nil
}

func cloneState(st State) State {
	if st.Alerts != nil {
		alerts := make(map[string]time.Time, len(st.Alerts))
		for k, v := range st.Alerts {
			alerts[k] = v
		}
		st.Alerts = alerts
	}
	return st
}

func readTail(f *os.File, id int64) (State, int64, error) {
	info, err := f.Stat()
	if err != nil {
		return State{}, 0, err
	}
	if info.Size() == 0 {
		return State{AccountID: id}, 0, nil
	}
	start := max(int64(0), info.Size()-2*maxRecord)
	raw := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(raw, start); err != nil {
		return State{}, 0, err
	}
	end := bytes.LastIndexByte(raw, '\n')
	if end < 0 {
		// Even the first reserved append may have been interrupted. Refuse
		// automatic reset rather than guessing whether a request was sent.
		return State{}, 0, errors.New("evidence journal has no committed record")
	}
	begin := bytes.LastIndexByte(raw[:end], '\n') + 1
	if begin == 0 && start != 0 {
		return State{}, 0, errors.New("evidence record exceeds tail limit")
	}
	var r record
	if err := json.Unmarshal(raw[begin:end], &r); err != nil || r.Version != 1 {
		return State{}, 0, errors.New("invalid evidence journal record")
	}
	hash := sha256.Sum256(r.State)
	var state State
	if r.Hash != hex.EncodeToString(hash[:]) || json.Unmarshal(r.State, &state) != nil ||
		state.AccountID != id || state.Step < 0 || state.Step > 5 || state.Sequence == 0 ||
		state.ConsecutiveFails < 0 || len(state.Alerts) > 3 {
		return State{}, 0, errors.New("corrupt evidence journal state")
	}
	switch state.Operation.Phase {
	case "", "reserved", "dispatched", "ok", "failed", "unknown":
	default:
		return State{}, 0, errors.New("invalid evidence operation phase")
	}
	return state, start + int64(end) + 1, nil
}

func (s *Store) Read(id int64) (State, error) {
	return s.update(id, func(st *State) bool {
		if st.Operation.Owner != s.owner && (st.Operation.Phase == "reserved" || st.Operation.Phase == "dispatched") {
			st.Operation.Phase = "unknown"
			fail(st, time.Now(), time.Time{})
			st.LastEvent = event(id, st.Operation.Cohort, time.Now(), "probe_result", "unknown", st)
			return true
		}
		return false
	})
}

func (s *Store) Snapshot() ([]State, error) {
	var ids []int64
	if s.memory != nil {
		s.mu.Lock()
		for id := range s.memory {
			ids = append(ids, id)
		}
		s.mu.Unlock()
	} else {
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".jsonl.lock") {
				id, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".jsonl.lock"), 10, 64)
				if err != nil || id <= 0 {
					return nil, errors.New("invalid evidence filename")
				}
				ids = append(ids, id)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	states := make([]State, 0, len(ids))
	for _, id := range ids {
		state, err := s.Read(id)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func (s *Store) Begin(id int64, cohort Cohort, now time.Time) (string, error) {
	var blocked error
	state, err := s.update(id, func(st *State) bool {
		switch st.Operation.Phase {
		case "unknown":
			if !st.Operation.Reconciled {
				blocked = ErrBlocked
				return false
			}
		case "reserved", "dispatched":
			blocked = ErrBlocked
			if st.Operation.Owner != s.owner {
				st.Operation.Phase = "unknown"
				fail(st, now, time.Time{})
				st.LastEvent = event(id, st.Operation.Cohort, now, "probe_result", "unknown", st)
				return true
			}
			return false
		}
		if st.Deadline().After(now) {
			blocked = ErrBackoff
			return false
		}
		st.Operation = Operation{ID: fmt.Sprintf("%s-%d-%d", s.owner, id, st.Sequence+1),
			Owner: s.owner, Phase: "reserved", Cohort: cohort}
		st.LastEvent = event(id, cohort, now, "probe_reserved", "reserved", st)
		return true
	})
	if err != nil {
		return "", err
	}
	if blocked != nil {
		return "", blocked
	}
	return state.Operation.ID, nil
}

func (s *Store) Dispatch(id int64, op string, c Cohort, now time.Time) error {
	var accepted bool
	_, err := s.update(id, func(st *State) bool {
		if st.Operation.ID != op || st.Operation.Phase != "reserved" || st.Operation.Owner != s.owner ||
			st.Operation.Cohort != c {
			return false
		}
		if st.Deadline().After(now) {
			return false
		}
		accepted = true
		st.Operation.Phase = "dispatched"
		st.LastEvent = event(id, c, now, "probe_dispatched", "dispatched", st)
		return true
	})
	if err == nil && !accepted {
		err = ErrBlocked
	}
	return err
}

func fail(st *State, now, retryUntil time.Time) {
	st.Step = min(st.Step+1, 5)
	delay := [...]time.Duration{0, 2, 4, 8, 16, 30}[st.Step] * time.Second
	st.NotBefore = later(st.NotBefore, later(now.Add(delay), retryUntil))
}

func succeed(st *State, now time.Time) {
	if !st.Deadline().After(now) && st.Step > 0 {
		st.Step--
	}
}

func (s *Store) Finish(id int64, op string, c Cohort, result string, now, retryUntil time.Time) error {
	switch result {
	case "pass", "failed", "unknown", "observed":
	default:
		return errors.New("invalid probe result")
	}
	var accepted bool
	_, err := s.update(id, func(st *State) bool {
		if st.Operation.ID != op || st.Operation.Owner != s.owner ||
			st.Operation.Cohort != c ||
			(st.Operation.Phase != "reserved" && st.Operation.Phase != "dispatched") {
			return false
		}
		if (result == "pass" || result == "observed") && st.Operation.Phase != "dispatched" {
			return false
		}
		accepted = true
		switch result {
		case "pass":
			st.Operation.Phase = "ok"
			st.ProbePassed++
			succeed(st, now)
		case "unknown":
			st.Operation.Phase = "unknown"
			fail(st, now, retryUntil)
		case "observed":
			// Host probe completion is not a graded pass.
			st.Operation.Phase = "ok"
		default:
			st.Operation.Phase = "failed"
			fail(st, now, retryUntil)
		}
		st.LastEvent = event(id, c, now, "probe_result", result, st)
		return true
	})
	if err == nil && !accepted {
		err = ErrBlocked
	}
	return err
}

func (s *Store) Policy(id int64, failures int, quality, retry time.Time, now time.Time) error {
	if failures < 0 {
		return errors.New("invalid failure count")
	}
	_, err := s.update(id, func(st *State) bool {
		quality = later(st.QualityUntil, quality)
		retry = later(st.NotBefore, retry)
		if st.ConsecutiveFails == failures && st.QualityUntil.Equal(quality) && st.NotBefore.Equal(retry) {
			return false
		}
		st.ConsecutiveFails, st.QualityUntil, st.NotBefore = failures, quality, retry
		st.LastEvent = event(id, st.Operation.Cohort, now, "backoff_policy", "retained", st)
		return true
	})
	return err
}

func (s *Store) Business(id int64, c Cohort, result string, now time.Time) error {
	switch result {
	case "dispatched", "succeeded", "failed", "unknown", "observed":
	default:
		return errors.New("invalid traffic result")
	}
	_, err := s.update(id, func(st *State) bool {
		switch result {
		case "dispatched":
			st.BusinessDispatched++
		case "succeeded":
			st.BusinessSucceeded++
			succeed(st, now)
		}
		st.LastEvent = event(id, c, now, "business_"+result, result, st)
		return true
	})
	return err
}

// Reconcile acknowledges an unknown operation without changing its terminal
// result, inventing success, clearing cooldown, or replaying its request.
// Only the explicit operator command calls this CAS-protected path.
func (s *Store) Reconcile(id int64, op string, now time.Time) error {
	accepted := false
	_, err := s.update(id, func(st *State) bool {
		if st.Operation.ID != op || st.Operation.Phase != "unknown" || st.Operation.Reconciled {
			return false
		}
		st.Operation.Reconciled = true
		st.LastEvent = event(id, st.Operation.Cohort, now, "probe_reconciled", "unknown_acknowledged", st)
		accepted = true
		return true
	})
	if err == nil && !accepted {
		return ErrBlocked
	}
	return err
}

// Signal persists every detection; alerts are once per account/category per
// rolling ten minutes, including across plugin reloads.
func (s *Store) Signal(id int64, c Cohort, category string, now time.Time) (bool, error) {
	switch category {
	case "turn_state_ticket", "state_rejection", "admission_rejected":
	default:
		return false, errors.New("invalid admission category")
	}
	alert := false
	_, err := s.update(id, func(st *State) bool {
		if st.Alerts == nil {
			st.Alerts = make(map[string]time.Time)
		}
		alert = !st.Alerts[category].After(now)
		if alert {
			st.Alerts[category] = now.Add(10 * time.Minute)
		}
		st.LastEvent = event(id, c, now, "admission_rejected", category, st)
		st.LastEvent.Alert = alert
		return true
	})
	return alert && err == nil, err
}

func event(id int64, c Cohort, now time.Time, kind, result string, st *State) Event {
	op := ""
	if strings.HasPrefix(kind, "probe_") {
		op = st.Operation.ID
	}
	return Event{Cohort: c, AccountID: id, At: now, Kind: kind, Result: result,
		Operation: op, Step: st.Step, Until: st.Deadline()}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
