package operations

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/resolver"
	"github.com/achmadss/hanse/internal/runtime"
	"github.com/achmadss/hanse/internal/validation"
)

// Config is what a Manager needs.
type Config struct {
	// Registry holds the loaded capabilities and nodes. Required.
	Registry *registry.Registry
	// Dispatcher runs the validated calls. Required.
	Dispatcher Dispatcher
	// Timeouts are the gateway.yaml defaults; zero fields take the
	// documented defaults (docs/configuration.md section 3.1).
	Timeouts config.Timeouts
	// Store keeps the records; nil means NewMemoryStore. Issue #17 passes
	// the SQLite store here.
	Store Store
}

// Manager accepts operation requests, validates them, records them,
// dispatches them to a runtime, tracks their state, and cancels them
// (docs/architecture.md sections 4 and 9).
type Manager struct {
	registry   *registry.Registry
	resolver   *resolver.Resolver
	dispatcher Dispatcher
	timeouts   config.Timeouts
	store      Store
	now        func() time.Time
	newID      func() string
	// mu serializes the state transitions: idempotency lookup,
	// concurrency check, insert, the queued-to-running move, completion,
	// and cancellation. Store operations are short, so one mutex is
	// enough; the dispatcher never runs under it.
	mu   sync.Mutex
	live map[string]*execution
}

// New builds a Manager. It fails with the Internal code when the registry
// or the dispatcher is missing, because a Gateway with either missing
// cannot run anything.
func New(cfg Config) (*Manager, error) {
	if cfg.Registry == nil {
		return nil, herrors.New(herrors.Internal,
			"the operations manager needs a capability registry", nil)
	}
	if cfg.Dispatcher == nil {
		return nil, herrors.New(herrors.Internal,
			"the operations manager needs a dispatcher", nil)
	}
	defaults := config.DefaultTimeouts()
	timeouts := cfg.Timeouts
	if timeouts.Operation <= 0 {
		timeouts.Operation = defaults.Operation
	}
	if timeouts.Options <= 0 {
		timeouts.Options = defaults.Options
	}
	if timeouts.GracePeriod <= 0 {
		timeouts.GracePeriod = defaults.GracePeriod
	}
	store := cfg.Store
	if store == nil {
		store = NewMemoryStore()
	}
	return &Manager{
		registry:   cfg.Registry,
		resolver:   resolver.New(cfg.Registry, cfg.Registry.Nodes),
		dispatcher: cfg.Dispatcher,
		timeouts:   timeouts,
		store:      store,
		now:        time.Now,
		newID:      newOperationID,
		live:       map[string]*execution{},
	}, nil
}

// execution is the in-memory state of one run that has not finished yet:
// the arguments the envelope needs, the elevation input, the cancel
// function, and the channel that closes when the run ends. None of it is
// stored.
type execution struct {
	args      json.RawMessage
	elevation *Elevation
	done      chan struct{}
	once      sync.Once
	mu        sync.Mutex
	cancel    context.CancelFunc
	// cancelled records that a client asked to stop the run.
	cancelled bool
}

// finish closes the done channel exactly once.
func (e *execution) finish() {
	e.once.Do(func() { close(e.done) })
}

// setCancel records the cancel function of the run's context.
func (e *execution) setCancel(cancel context.CancelFunc) {
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()
}

// stop cancels the run's context.
func (e *execution) stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// requestCancellation marks that a client asked to stop the run.
func (e *execution) requestCancellation() {
	e.mu.Lock()
	e.cancelled = true
	e.mu.Unlock()
}

// cancellationRequested reports whether a client asked to stop the run.
func (e *execution) cancellationRequested() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelled
}

// Validate runs the checks of the dry-run endpoint: it looks up the
// operation, validates the arguments, and resolves the executor without
// creating or running anything (docs/architecture.md section 4).
func (m *Manager) Validate(req Request) (Check, error) {
	_, op, res, err := m.prepare(req)
	if err != nil {
		return Check{}, err
	}
	return Check{
		Executor:       res.Executor,
		ExecutorSource: string(res.Source),
		Mode:           op.Mode,
	}, nil
}

// Submit accepts one request. A synchronous operation runs before Submit
// returns, and the returned record holds the result. An asynchronous
// operation returns at once with the record in the queued state.
//
// A repeated Idempotency-Key from the same requester returns the original
// operation without running it again; a synchronous retry waits for the
// original run and returns its final record. The same key with different
// arguments is the InvalidArguments code, because the key cannot stand for
// two different requests.
func (m *Manager) Submit(req Request) (Record, error) {
	_, op, res, err := m.prepare(req)
	if err != nil {
		return Record{}, err
	}
	values, fingerprint, err := canonicalArguments(req.Arguments)
	if err != nil {
		return Record{}, herrors.New(herrors.InvalidArguments,
			"the arguments are not a JSON object",
			map[string]any{"errors": []any{map[string]any{
				"path": "", "rule": "json", "message": "the arguments are not a JSON object",
			}}})
	}
	key := concurrencyKey(op.Concurrency, values)

	m.mu.Lock()
	if req.IdempotencyKey != "" {
		if prior, ok := m.store.ByIdempotency(req.Requester, req.IdempotencyKey); ok {
			if prior.ArgumentFingerprint != fingerprint {
				m.mu.Unlock()
				return Record{}, herrors.New(herrors.InvalidArguments,
					"the Idempotency-Key was already used with different arguments",
					map[string]any{
						"idempotency_key": req.IdempotencyKey,
						"operation_id":    prior.ID,
					})
			}
			exec := m.live[prior.ID]
			m.mu.Unlock()
			return m.await(prior, exec, op.Mode == registry.ModeSync), nil
		}
	}
	if running, ok := m.conflict(req.Capability, op, key); ok {
		m.mu.Unlock()
		return Record{}, herrors.New(herrors.OperationConflict,
			fmt.Sprintf("operation %q is already running", running.ID),
			map[string]any{
				"capability":   req.Capability,
				"operation":    req.Operation,
				"operation_id": running.ID,
			})
	}
	rec := Record{
		ID:                  m.newID(),
		Capability:          req.Capability,
		Operation:           req.Operation,
		Requester:           req.Requester,
		Executor:            res.Executor,
		ExecutorSource:      string(res.Source),
		Mode:                op.Mode,
		Status:              StateQueued,
		CreatedAt:           m.now().UTC(),
		IdempotencyKey:      req.IdempotencyKey,
		ArgumentFingerprint: fingerprint,
		Arguments:           redactArguments(op, req.Arguments, passwordOf(req.Elevation)),
		ConcurrencyKey:      key,
	}
	if err := m.store.Insert(rec); err != nil {
		m.mu.Unlock()
		return Record{}, err
	}
	exec := &execution{
		args:      append(json.RawMessage(nil), req.Arguments...),
		elevation: req.Elevation,
		done:      make(chan struct{}),
	}
	m.live[rec.ID] = exec
	m.mu.Unlock()

	if op.Mode == registry.ModeAsync {
		go m.execute(exec, rec, op)
		return rec, nil
	}
	m.execute(exec, rec, op)
	if done, ok := m.store.Get(rec.ID); ok {
		return done, nil
	}
	return rec, nil
}

// await waits for a duplicate idempotent request's original run when the
// operation is synchronous, so the retry returns the result rather than a
// queued record.
func (m *Manager) await(rec Record, exec *execution, wait bool) Record {
	if !wait || rec.Status.Final() || exec == nil {
		return rec
	}
	<-exec.done
	if done, ok := m.store.Get(rec.ID); ok {
		return done
	}
	return rec
}

// Get returns one operation.
func (m *Manager) Get(id string) (Record, error) {
	rec, ok := m.store.Get(id)
	if !ok {
		return Record{}, unknownOperation(id)
	}
	return rec, nil
}

// List returns the operations that satisfy f, sorted by operation ID.
func (m *Manager) List(f Filter) []Record {
	return m.store.List(f)
}

// RecoverInterrupted fails every operation that a previous Gateway process
// left queued or running. A store that does not survive a restart has
// nothing to recover, so the call succeeds and reports zero.
func (m *Manager) RecoverInterrupted() (int, error) {
	recoverer, ok := m.store.(Recoverer)
	if !ok {
		return 0, nil
	}
	return recoverer.RecoverInterrupted(m.now().UTC())
}

// PurgeOld removes the finished operations older than the configured
// retention. The history store runs this itself on startup and on a timer;
// this method lets the Gateway trigger it, for example after a test or an
// operator request. A store without retention returns zero.
func (m *Manager) PurgeOld() (int64, error) {
	pruner, ok := m.store.(Pruner)
	if !ok {
		return 0, nil
	}
	return pruner.PurgeOld(m.now().UTC())
}

// Cancel cancels one operation (docs/architecture.md section 9.5). A
// queued operation becomes cancelled at once. A running operation gets a
// stop request through its context; Cancel waits for the grace period and
// returns the record, which a fast handler has already moved to cancelled.
// An operation that already ended is returned unchanged, so a repeated
// cancel is safe.
func (m *Manager) Cancel(id string) (Record, error) {
	m.mu.Lock()
	rec, ok := m.store.Get(id)
	if !ok {
		m.mu.Unlock()
		return Record{}, unknownOperation(id)
	}
	exec := m.live[id]
	switch {
	case rec.Status == StateQueued:
		rec.Status = StateCancelled
		rec.FinishedAt = m.now().UTC()
		if err := m.store.Update(rec); err != nil {
			m.mu.Unlock()
			return Record{}, err
		}
		if exec != nil {
			exec.finish()
			delete(m.live, id)
		}
		m.mu.Unlock()
		return rec, nil
	case rec.Status == StateRunning && exec != nil:
		exec.requestCancellation()
		grace := m.gracePeriod(rec)
		m.mu.Unlock()
		exec.stop()
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-exec.done:
		case <-timer.C:
		}
		if done, ok := m.store.Get(id); ok {
			return done, nil
		}
		return rec, nil
	default:
		m.mu.Unlock()
		return rec, nil
	}
}

// prepare runs the checks shared by Submit and Validate: lookup,
// argument validation, and executor resolution.
func (m *Manager) prepare(req Request) (*registry.Capability, *registry.Operation, resolver.Resolution, error) {
	if req.Capability == "" {
		return nil, nil, resolver.Resolution{}, herrors.New(herrors.InvalidRequest,
			"the request needs a capability ID", map[string]any{"field": "capability"})
	}
	if req.Operation == "" {
		return nil, nil, resolver.Resolution{}, herrors.New(herrors.InvalidRequest,
			"the request needs an operation ID", map[string]any{"field": "operation"})
	}
	c, ok := m.registry.Capability(req.Capability)
	if !ok {
		return nil, nil, resolver.Resolution{}, herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("capability %q does not exist", req.Capability),
			map[string]any{"capability": req.Capability})
	}
	op, ok := c.Operation(req.Operation)
	if !ok {
		return nil, nil, resolver.Resolution{}, herrors.New(herrors.UnknownOperation,
			fmt.Sprintf("capability %q has no operation %q", req.Capability, req.Operation),
			map[string]any{"capability": req.Capability, "operation": req.Operation})
	}
	if err := validation.Validate(op, m.registry.Nodes, req.Arguments); err != nil {
		return nil, nil, resolver.Resolution{}, err
	}
	res, err := m.resolver.Resolve(req.Capability, req.Operation, req.Executor)
	if err != nil {
		return nil, nil, resolver.Resolution{}, err
	}
	return c, op, res, nil
}

// conflict returns the active operation that the concurrency setting
// forbids running alongside a new one, docs/architecture.md section 9.4.
// Runs are counted per concurrency key; a queued run counts too, because
// it is about to run.
func (m *Manager) conflict(capID string, op *registry.Operation, key string) (Record, bool) {
	if op.Concurrency == nil || op.Concurrency.Limit <= 0 {
		return Record{}, false
	}
	running := 0
	first := Record{}
	for _, rec := range m.store.List(Filter{Capability: capID, Operation: op.ID}) {
		if !rec.Status.active() || rec.ConcurrencyKey != key {
			continue
		}
		if running == 0 {
			first = rec
		}
		running++
	}
	if running < op.Concurrency.Limit {
		return Record{}, false
	}
	return first, true
}

// execute runs one accepted operation. It moves the record to running,
// dispatches the call, and records the final state.
func (m *Manager) execute(exec *execution, rec Record, op *registry.Operation) {
	defer m.release(exec, rec.ID)

	// The cancel function is registered before the record becomes
	// running, so a cancel that arrives the moment the run starts can
	// never find a nil one and be lost.
	timeout := config.ResolveTimeouts(m.timeouts, &config.OperationTimeouts{
		Timeout:     op.Timeout,
		GracePeriod: op.GracePeriod,
	}).Operation
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	exec.setCancel(cancel)
	defer cancel()

	m.mu.Lock()
	current, ok := m.store.Get(rec.ID)
	if !ok || current.Status != StateQueued {
		// Cancelled while it waited to start.
		m.mu.Unlock()
		return
	}
	start := m.now()
	current.Status = StateRunning
	current.StartedAt = start.UTC()
	if err := m.store.Update(current); err != nil {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// A cancel that landed after the running update still stops the
	// handler through the registered cancel function.
	if exec.cancellationRequested() {
		cancel()
	}

	envelope := m.envelope(current, op, start, timeout, exec.args)
	// The elevation input lives only for the dispatch. The password is
	// kept for the length of the call only, so the stored result and
	// failure cannot echo it (docs/architecture.md section 8.5).
	elevation := exec.elevation
	exec.elevation = nil
	password := passwordOf(elevation)
	resp, err := m.dispatcher.Execute(ctx, &Call{Request: envelope, Elevation: elevation})
	m.complete(ctx, current.ID, resp, err, exec.cancellationRequested(), password)
}

// release closes the run's done channel and drops its in-memory state.
func (m *Manager) release(exec *execution, id string) {
	m.mu.Lock()
	if m.live[id] == exec {
		delete(m.live, id)
	}
	m.mu.Unlock()
	exec.finish()
}

// complete stores the final state of one run. A record that already
// reached a final state keeps it: only a queued operation is finalized
// outside the run.
func (m *Manager) complete(ctx context.Context, id string, resp runtime.Response, err error, cancelRequested bool, password string) {
	state, failure := classify(ctx, resp, err, cancelRequested)

	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.store.Get(id)
	if !ok || rec.Status.Final() {
		return
	}
	rec.Status = state
	rec.FinishedAt = m.now().UTC()
	if state == StateCompleted || state == StateCancelled {
		rec.Result = append(json.RawMessage(nil), redactResult(resp.Result, password)...)
		rec.Summary = redactText(resp.Summary, password)
	}
	rec.Failure = redactFailure(failure, password)
	_ = m.store.Update(rec)
}

// classify turns one dispatch result into the operation's final state and
// its failure, following the first matching row of the final-state table
// in docs/handler-contract.md section 5.
func classify(ctx context.Context, resp runtime.Response, err error, cancelRequested bool) (State, *Failure) {
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if err != nil {
		e := herrors.From(err)
		switch {
		case timedOut:
			return StateFailed, timeoutFailure()
		case cancelRequested:
			return StateCancelled, nil
		default:
			return StateFailed, &Failure{
				Code:    e.Code().Name(),
				Message: e.Message,
				Details: e.Details,
			}
		}
	}
	if timedOut && resp.Status != runtime.StatusOK {
		return StateFailed, timeoutFailure()
	}
	switch resp.Status {
	case runtime.StatusOK:
		return StateCompleted, nil
	case runtime.StatusCancelled:
		if cancelRequested {
			return StateCancelled, nil
		}
		return StateFailed, &Failure{
			Code:    herrors.HandlerOutputInvalid.Name(),
			Message: "the handler cancelled a run that nobody asked to cancel",
		}
	case runtime.StatusError:
		if resp.Err == nil {
			return StateFailed, &Failure{
				Code:    herrors.HandlerOutputInvalid.Name(),
				Message: "the handler reported an error without an error object",
			}
		}
		return StateFailed, &Failure{
			Code:    resp.Err.Code,
			Message: resp.Err.Message,
			Details: resp.Err.Details,
		}
	default:
		return StateFailed, &Failure{
			Code:    herrors.HandlerOutputInvalid.Name(),
			Message: fmt.Sprintf("the handler returned status %q", resp.Status),
		}
	}
}

// timeoutFailure is the failure of a run that exceeded its deadline.
func timeoutFailure() *Failure {
	return &Failure{
		Code:    herrors.Timeout.Name(),
		Message: "the operation exceeded its deadline",
	}
}

// envelope builds the handler request envelope of one run
// (docs/handler-contract.md section 2).
func (m *Manager) envelope(rec Record, op *registry.Operation, start time.Time, timeout time.Duration, args json.RawMessage) *runtime.Request {
	req := &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         rec.ID,
		Capability: rec.Capability,
		Operation:  rec.Operation,
		Arguments:  append(json.RawMessage(nil), args...),
		Nodes:      m.referencedNodes(op, args),
		Deadline:   config.HandlerDeadline(start, timeout),
	}
	if view, ok := m.registry.Nodes.Get(rec.Executor); ok {
		req.Executor = runtimeNode(view)
	}
	return req
}

// referencedNodes returns every node that a `hanse:node` argument refers
// to, keyed by node ID (docs/handler-contract.md section 2.1). The filters
// of the operation name the arguments; a value can sit inside a nested
// object or an array.
func (m *Manager) referencedNodes(op *registry.Operation, args json.RawMessage) map[string]runtime.Node {
	out := map[string]runtime.Node{}
	if len(op.NodeFilters) == 0 || len(bytes.TrimSpace(args)) == 0 {
		return out
	}
	var values map[string]any
	if err := json.Unmarshal(args, &values); err != nil {
		return out
	}
	for field := range op.NodeFilters {
		collectNodeIDs(values[field], func(id string) {
			if view, ok := m.registry.Nodes.Get(id); ok {
				out[id] = runtimeNode(view)
			}
		})
	}
	return out
}

// collectNodeIDs calls visit for every string in value that names a known
// node. It walks objects and arrays, because a node argument can be nested.
func collectNodeIDs(value any, visit func(id string)) {
	switch v := value.(type) {
	case string:
		visit(v)
	case []any:
		for _, item := range v {
			collectNodeIDs(item, visit)
		}
	case map[string]any:
		for _, item := range v {
			collectNodeIDs(item, visit)
		}
	}
}

// runtimeNode renders one registry node as the envelope's node object. The
// registry holds no secret values, so nothing here needs redaction.
func runtimeNode(view registry.NodeView) runtime.Node {
	attributes := make(map[string]any, len(view.Attributes))
	for name, value := range view.Attributes {
		attributes[name] = value
	}
	return runtime.Node{
		ID:         view.ID,
		OS:         view.OS,
		Arch:       view.Arch,
		Attributes: attributes,
	}
}

// gracePeriod returns the effective grace period of one operation.
func (m *Manager) gracePeriod(rec Record) time.Duration {
	c, ok := m.registry.Capability(rec.Capability)
	if !ok {
		return m.timeouts.GracePeriod
	}
	op, ok := c.Operation(rec.Operation)
	if !ok {
		return m.timeouts.GracePeriod
	}
	return config.ResolveTimeouts(m.timeouts, &config.OperationTimeouts{
		Timeout:     op.Timeout,
		GracePeriod: op.GracePeriod,
	}).GracePeriod
}

// canonicalArguments decodes the argument object and returns its canonical
// JSON fingerprint. Two requests with the same arguments in another key
// order or with other whitespace share one fingerprint, and no argument
// value is stored.
func canonicalArguments(raw json.RawMessage) (map[string]any, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]any{}, fingerprint([]byte("{}")), nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(values)
	if err != nil {
		return nil, "", err
	}
	return values, fingerprint(canonical), nil
}

// fingerprint returns the hex SHA-256 of data.
func fingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// concurrencyKey returns the combination of `concurrency.key` argument
// values of one run, or "" when the operation sets no limit. Without key
// arguments every run of the operation shares one bucket.
func concurrencyKey(c *registry.Concurrency, values map[string]any) string {
	if c == nil || c.Limit <= 0 {
		return ""
	}
	parts := make([]any, len(c.Key))
	for i, name := range c.Key {
		parts[i] = values[name]
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// unknownOperation is the error of an operation ID that is not stored. The
// documented code set has no separate code for an unknown operation run,
// and UnknownOperation already reports an operation that does not exist.
func unknownOperation(id string) error {
	return herrors.New(herrors.UnknownOperation,
		fmt.Sprintf("operation %q does not exist", id),
		map[string]any{"operation_id": id})
}

// newOperationID returns a fresh operation ID: the `op_` prefix and 128
// random bits in hex.
func newOperationID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand never fails on the supported platforms; a broken
		// source is a fatal condition for an ID, not a retryable error.
		panic(fmt.Sprintf("operations: cannot generate an operation ID: %v", err))
	}
	return "op_" + hex.EncodeToString(raw[:])
}
