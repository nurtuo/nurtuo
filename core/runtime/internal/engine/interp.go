// SPDX-License-Identifier: AGPL-3.0-only

package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// FlowResolver loads a flow by id (and optional revision) for ProcessCall.
type FlowResolver interface {
	Resolve(ctx context.Context, id string, revision *int) (*flow.Flow, error)
}

// MapResolver is a FlowResolver over an in-memory set of flows (revision is ignored).
type MapResolver map[string]*flow.Flow

func (m MapResolver) Resolve(_ context.Context, id string, _ *int) (*flow.Flow, error) {
	if f, ok := m[id]; ok {
		return f, nil
	}
	return nil, fmt.Errorf("flow %q not found", id)
}

// DefaultMaxDepth bounds nested ProcessCall recursion.
const DefaultMaxDepth = 16

// Engine interprets validated flows set-at-a-time. Documents at a step form one set that is
// processed by the step as a whole; Decision/Route split the set across ports, Branch runs its
// paths sequentially over copies of the set. Returned documents are collected in execution order
// (port order: decision true before false, branch path 1..n, route ids in declared order).
type Engine struct {
	Registry             *Registry
	Resolver             FlowResolver // optional; required for ProcessCall
	Eval                 Evaluator    // defaults to BasicEvaluator; see CELEvaluator
	Spool                *docs.Spool  // defaults to an in-memory-first spool in os temp
	MaxDepth             int
	MaxReturnedDocuments int   // zero is unlimited
	MaxReturnedBytes     int64 // decoded payload bytes; zero is unlimited
}

// New returns an Engine with built-in Message/SetProperties/Map runners (BasicEvaluator; set Eval to &CELEvaluator{} for full CEL).
func New() *Engine {
	return &Engine{Registry: NewRegistry(), Eval: BasicEvaluator{}, Spool: docs.NewSpool(docs.SpoolConfig{}), MaxDepth: DefaultMaxDepth}
}

// ErrReturnLimit means the configured return document or byte budget was exceeded.
var ErrReturnLimit = errors.New("engine: return limit exceeded")

// StopInfo is set when execution ended at a Stop step.
type StopInfo struct {
	StepID  string
	Status  int // 0 when unset
	Message string
}

// StepTrace records what one step execution saw; Out counts documents sent on to the next step(s).
type StepTrace struct {
	StepID  string
	Kind    string
	In      int
	Out     int
	Failed  int
	Dropped int
}

// Result is the outcome of one flow execution. The caller owns Returned and Failed and must Close them.
type Result struct {
	Returned *docs.Set   // documents that reached a Return step
	Failed   *docs.Set   // documents that errored outside any Try/Catch (status error)
	Stop     *StopInfo   // non-nil when a Stop step ended the execution
	Trace    []StepTrace // deterministic per-step statuses in execution order
}

// Run executes f over in. in is consumed (ownership passes to the engine). On ctx cancellation the
// partial Result is returned together with ctx.Err().
func (e *Engine) Run(ctx context.Context, f *flow.Flow, in *docs.Set, process *Process) (*Result, error) {
	if process == nil {
		process = NewProcess(nil)
	}
	return e.run(ctx, f, in, process, 0)
}

func (e *Engine) run(ctx context.Context, f *flow.Flow, in *docs.Set, proc *Process, depth int) (*Result, error) {
	x := &exec{e: e, f: f, proc: proc, depth: depth, steps: map[string]flow.Step{}, ret: docs.NewSet(), failed: docs.NewSet()}
	for _, s := range f.Steps {
		x.steps[s.ID] = s
	}
	res := &Result{Returned: x.ret, Failed: x.failed}
	start := ""
	for _, s := range f.Steps {
		if s.Kind == flow.KindStart {
			start = s.ID
			break
		}
	}
	if start == "" {
		_ = in.Close()
		return res, errors.New("engine: flow has no start step")
	}
	err := x.walk(ctx, start, in)
	res.Stop, res.Trace = x.stop, x.trace
	return res, err
}

type frame struct{ failed *docs.Set }

type exec struct {
	e             *Engine
	f             *flow.Flow
	proc          *Process
	depth         int
	steps         map[string]flow.Step
	ret           *docs.Set
	failed        *docs.Set
	stop          *StopInfo
	frames        []*frame
	trace         []StepTrace
	returnedBytes int64
}

type portSet struct {
	port string
	set  *docs.Set
}

func (x *exec) fail(d *docs.Document) {
	if n := len(x.frames); n > 0 {
		x.frames[n-1].failed.Add(d)
		return
	}
	x.failed.Add(d)
}

// triage routes errored documents to the nearest Try/Catch frame (or the result) and discards
// dropped ones; it returns the OK documents and the number failed / dropped.
func (x *exec) triage(s *docs.Set) (ok *docs.Set, failed, dropped int) {
	ok = docs.NewSet()
	for _, d := range s.Docs() {
		switch d.Status() {
		case docs.StatusOK:
			ok.Add(d)
		case docs.StatusError:
			failed++
			x.fail(d)
		default:
			dropped++
			_ = d.Release()
		}
	}
	return
}

func (x *exec) failAll(s *docs.Set, e *docs.Error) {
	for _, d := range s.Docs() {
		if d.Status() == docs.StatusOK {
			d.Fail(e)
		}
	}
}

func (x *exec) next(step flow.Step, port string) (string, error) {
	to, ok := x.f.Edges[step.ID+":"+port]
	if !ok {
		return "", fmt.Errorf("engine: step %s has no edge for port %q", step.ID, port)
	}
	if _, ok := x.steps[to]; !ok {
		return "", fmt.Errorf("engine: edge %s:%s targets unknown step %q", step.ID, port, to)
	}
	return to, nil
}

func (x *exec) collectReturned(ok *docs.Set) error {
	if limit := x.e.MaxReturnedDocuments; limit > 0 && ok.Len() > limit-x.ret.Len() {
		_ = ok.Close()
		return ErrReturnLimit
	}
	if limit := x.e.MaxReturnedBytes; limit > 0 {
		for _, d := range ok.Docs() {
			if p := d.Payload(); p != nil {
				if p.Size() > limit-x.returnedBytes {
					_ = ok.Close()
					return ErrReturnLimit
				}
				x.returnedBytes += p.Size()
			}
		}
	}
	for _, d := range ok.Docs() {
		x.ret.Add(d)
	}
	return nil
}

// walk runs the set through step id and on, following default ports iteratively and recursing at
// splits. It always consumes (releases or hands off) the set.
func (x *exec) walk(ctx context.Context, id string, set *docs.Set) error {
	for {
		if err := ctx.Err(); err != nil {
			_ = set.Close()
			return err
		}
		if x.stop != nil {
			return set.Close()
		}
		step, ok := x.steps[id]
		if !ok {
			_ = set.Close()
			return fmt.Errorf("engine: unknown step %q", id)
		}
		x.trace = append(x.trace, StepTrace{StepID: id, Kind: step.Kind, In: set.Len()})
		ti := len(x.trace) - 1
		cfg, err := step.TypedConfig()
		if err != nil {
			_ = set.Close()
			return err
		}

		switch step.Kind {
		case flow.KindStop:
			sc := cfg.(flow.StopConfig)
			x.stop = &StopInfo{StepID: id, Message: sc.Message}
			if sc.Status != nil {
				x.stop.Status = *sc.Status
			}
			return set.Close()
		case flow.KindReturnDocuments:
			ok, f, dr := x.triage(set)
			if err := x.collectReturned(ok); err != nil {
				return err
			}
			x.trace[ti].Out, x.trace[ti].Failed, x.trace[ti].Dropped = ok.Len(), f, dr
			return nil
		case flow.KindDecision:
			return x.decision(ctx, step, cfg.(flow.DecisionConfig), set, ti)
		case flow.KindRoute:
			return x.route(ctx, step, cfg.(flow.RouteConfig), set, ti)
		case flow.KindBranch:
			return x.branch(ctx, step, cfg.(flow.BranchConfig), set, ti)
		case flow.KindTryCatch:
			return x.tryCatch(ctx, step, cfg.(flow.TryCatchConfig), set, ti)
		}

		// Single-output steps.
		var out *docs.Set
		if step.Kind == flow.KindProcessCall {
			out, err = x.processCall(ctx, step, cfg.(flow.ProcessCallConfig), set)
		} else {
			out, err = x.runStep(ctx, step, cfg, set)
		}
		ok2, f, dr := x.triage(out)
		x.trace[ti].Out, x.trace[ti].Failed, x.trace[ti].Dropped = ok2.Len(), f, dr
		if err != nil {
			_ = ok2.Close()
			return err
		}
		to, err := x.next(step, "default")
		if err != nil {
			_ = ok2.Close()
			return err
		}
		id, set = to, ok2
	}
}

func (x *exec) runStep(ctx context.Context, step flow.Step, cfg any, set *docs.Set) (*docs.Set, error) {
	if step.Kind == flow.KindStart {
		if _, ok := x.e.Registry.m[flow.KindStart]; !ok {
			return set, nil
		}
	}
	sr, err := x.e.Registry.lookup(step.Kind)
	if err != nil {
		x.failAll(set, &docs.Error{Step: step.ID, Code: "no_runner", Message: err.Error(), Cause: err})
		return set, nil
	}
	sc := &StepContext{Flow: x.f, Step: step, Config: cfg, Process: x.proc, Eval: x.e.Eval, Spool: x.e.Spool}
	out, err := sr.Run(ctx, sc, set)
	if out == nil {
		out = set
	}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		var de *docs.Error
		if !errors.As(err, &de) {
			de = &docs.Error{Step: step.ID, Message: err.Error(), Cause: err}
		}
		x.failAll(out, de)
	}
	return out, nil
}

// fanOut runs each non-empty port set down its edge, in order.
func (x *exec) fanOut(ctx context.Context, step flow.Step, parts []portSet) error {
	for i, p := range parts {
		if p.set.Len() == 0 {
			continue
		}
		to, err := x.next(step, p.port)
		if err != nil {
			for _, q := range parts[i:] {
				_ = q.set.Close()
			}
			return err
		}
		if err := x.walk(ctx, to, p.set); err != nil {
			for _, q := range parts[i+1:] {
				_ = q.set.Close()
			}
			return err
		}
	}
	return nil
}

func (x *exec) decision(ctx context.Context, step flow.Step, cfg flow.DecisionConfig, set *docs.Set, ti int) error {
	t, f := docs.NewSet(), docs.NewSet()
	failed := 0
	for _, d := range set.Docs() {
		if d.Status() != docs.StatusOK {
			continue
		}
		v, err := x.e.Eval.Condition(cfg.CEL, d, x.proc)
		switch {
		case err != nil:
			d.Fail(&docs.Error{Step: step.ID, Code: "condition", Message: err.Error(), Cause: err})
		case v:
			t.Add(d)
		default:
			f.Add(d)
		}
	}
	ok, nf, dr := x.triage(set) // errored/dropped docs from set
	_ = ok                      // OK docs are held by t/f; ok only aliases them
	failed, x.trace[ti].Dropped = nf, dr
	x.trace[ti].Failed, x.trace[ti].Out = failed, t.Len()+f.Len()
	return x.fanOut(ctx, step, []portSet{{"true", t}, {"false", f}})
}

func (x *exec) route(ctx context.Context, step flow.Step, cfg flow.RouteConfig, set *docs.Set, ti int) error {
	ports := flow.PortsOf(step)
	byPort := map[string]*docs.Set{}
	for _, p := range ports {
		byPort[p] = docs.NewSet()
	}
	for _, d := range set.Docs() {
		if d.Status() != docs.StatusOK {
			continue
		}
		target := "default"
		for _, r := range cfg.Routes {
			v, err := x.e.Eval.Condition(r.CEL, d, x.proc)
			if err != nil {
				d.Fail(&docs.Error{Step: step.ID, Code: "condition", Message: err.Error(), Cause: err})
				target = ""
				break
			}
			if v {
				target = r.ID
				break
			}
		}
		if target != "" {
			byPort[target].Add(d)
		}
	}
	_, nf, dr := x.triage(set)
	parts := make([]portSet, 0, len(ports))
	out := 0
	for _, p := range ports {
		out += byPort[p].Len()
		parts = append(parts, portSet{p, byPort[p]})
	}
	x.trace[ti].Out, x.trace[ti].Failed, x.trace[ti].Dropped = out, nf, dr
	return x.fanOut(ctx, step, parts)
}

func cloneSet(s *docs.Set) *docs.Set {
	c := docs.NewSet()
	for _, d := range s.Docs() {
		c.Add(d.Clone(d.ID))
	}
	return c
}

func (x *exec) branch(ctx context.Context, step flow.Step, cfg flow.BranchConfig, set *docs.Set, ti int) error {
	ok, nf, dr := x.triage(set)
	x.trace[ti].Out, x.trace[ti].Failed, x.trace[ti].Dropped = ok.Len(), nf, dr
	ports := flow.PortsOf(step)
	parts := make([]portSet, len(ports))
	for i, p := range ports {
		if i == len(ports)-1 {
			parts[i] = portSet{p, ok}
		} else {
			parts[i] = portSet{p, cloneSet(ok)}
		}
	}
	return x.fanOut(ctx, step, parts)
}

func (x *exec) tryCatch(ctx context.Context, step flow.Step, cfg flow.TryCatchConfig, set *docs.Set, ti int) error {
	ok, nf, dr := x.triage(set)
	x.trace[ti].Out, x.trace[ti].Failed, x.trace[ti].Dropped = ok.Len(), nf, dr
	tryTo, err := x.next(step, "try")
	if err != nil {
		_ = ok.Close()
		return err
	}
	catchTo, err := x.next(step, "catch")
	if err != nil {
		_ = ok.Close()
		return err
	}
	// Backups (shared payloads) let a retry re-run failed documents from their original state.
	var backup map[string]*docs.Document
	if cfg.Retries > 0 {
		backup = map[string]*docs.Document{}
		for _, d := range ok.Docs() {
			backup[d.ID] = d.Clone(d.ID)
		}
		defer func() {
			for _, b := range backup {
				_ = b.Release()
			}
		}()
	}
	var failed *docs.Set
	cur := ok
	for attempt := 0; ; attempt++ {
		fr := &frame{failed: docs.NewSet()}
		x.frames = append(x.frames, fr)
		err := x.walk(ctx, tryTo, cur)
		x.frames = x.frames[:len(x.frames)-1]
		failed = fr.failed
		if err != nil {
			_ = failed.Close()
			return err
		}
		if x.stop != nil {
			return failed.Close()
		}
		if failed.Len() == 0 || attempt >= cfg.Retries {
			break
		}
		cur = docs.NewSet()
		for _, d := range failed.Docs() {
			if b, ok := backup[d.ID]; ok {
				cur.Add(b.Clone(d.ID))
			}
			_ = d.Release()
		}
	}
	if failed.Len() == 0 {
		return nil
	}
	for _, d := range failed.Docs() {
		if e := d.ClearError(); e != nil {
			d.Props["nurtuo.error.message"] = e.Message
			d.Props["nurtuo.error.step"] = e.Step
			d.Props["nurtuo.error.code"] = e.Code
		}
	}
	return x.walk(ctx, catchTo, failed)
}

func (x *exec) processCall(ctx context.Context, step flow.Step, cfg flow.ProcessCallConfig, set *docs.Set) (*docs.Set, error) {
	fail := func(code string, err error) (*docs.Set, error) {
		x.failAll(set, &docs.Error{Step: step.ID, Code: code, Message: err.Error(), Cause: err})
		return set, nil
	}
	max := x.e.MaxDepth
	if max <= 0 {
		max = DefaultMaxDepth
	}
	if x.depth+1 > max {
		return fail("processCall.depth", fmt.Errorf("process call depth exceeds %d", max))
	}
	if x.e.Resolver == nil {
		return fail("processCall.resolve", errors.New("no flow resolver configured"))
	}
	child, err := x.e.Resolver.Resolve(ctx, cfg.Flow, cfg.Revision)
	if err != nil {
		return fail("processCall.resolve", err)
	}
	ok, out := docs.NewSet(), docs.NewSet()
	for _, d := range set.Docs() { // already-failed docs pass through untouched
		if d.Status() == docs.StatusOK {
			ok.Add(d)
		} else {
			out.Add(d)
		}
	}
	res, err := x.e.run(ctx, child, ok, NewProcess(x.proc.Props()), x.depth+1)
	for _, d := range res.Returned.Docs() {
		out.Add(d)
	}
	for _, d := range res.Failed.Docs() {
		out.Add(d) // status error: surfaces to the caller's Try/Catch
	}
	return out, err
}
