package jasper

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
)

type localProcess struct {
	proc  Process
	mutex sync.RWMutex
}

func (p *localProcess) ID() string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	return p.proc.ID()
}

func (p *localProcess) Info(ctx context.Context) ProcessInfo {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	return p.proc.Info(ctx)
}

func (p *localProcess) Running(ctx context.Context) bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	return p.proc.Running(ctx)
}

func (p *localProcess) Complete(ctx context.Context) bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	return p.proc.Complete(ctx)
}

func (p *localProcess) Signal(ctx context.Context, sig syscall.Signal) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	return errors.WithStack(p.proc.Signal(ctx, sig))
}

func (p *localProcess) Tag(t string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.proc.Tag(t)
}

func (p *localProcess) ResetTags() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.proc.ResetTags()
}

func (p *localProcess) GetTags() []string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	return p.proc.GetTags()
}

func (p *localProcess) RegisterTrigger(trigger ProcessTrigger) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	return errors.WithStack(p.proc.RegisterTrigger(trigger))
}

func (p *localProcess) Wait(ctx context.Context) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	return errors.WithStack(p.proc.Wait(ctx))
}

type basicProcess struct {
	id       string
	hostname string
	opts     CreateOptions
	cmd      *exec.Cmd
	tags     map[string]struct{}
	triggers ProcessTriggerSequence
}

func newBasicProcess(ctx context.Context, opts *CreateOptions) (Process, error) {
	cmd, err := opts.Resolve(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "problem building command from options")
	}

	// don't check the error here, if this fails, and we're
	// interested in the outcome, we'll see that later.
	_ = cmd.Start()

	opts.started = true

	p := &basicProcess{
		id:   uuid.Must(uuid.NewV4()).String(),
		opts: *opts,
		cmd:  cmd,
		tags: make(map[string]struct{}),
	}
	p.hostname, _ = os.Hostname()

	for _, t := range opts.Tags {
		p.Tag(t)
	}

	return p, nil
}

func (p *basicProcess) ID() string { return p.id }
func (p *basicProcess) Info(ctx context.Context) ProcessInfo {
	info := ProcessInfo{
		ID:        p.id,
		Options:   p.opts,
		Host:      p.hostname,
		Complete:  p.Complete(ctx),
		IsRunning: p.Running(ctx),
	}

	if info.Complete {
		info.Successful = p.cmd.ProcessState.Success()
		info.PID = -1
	}

	if info.IsRunning {
		info.PID = p.cmd.Process.Pid
	}

	return info
}

func (p *basicProcess) Complete(ctx context.Context) bool {
	if p.cmd == nil {
		return false
	}

	if p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited() {
		return true
	}

	return p.cmd.Process.Pid == -1
}

func (p *basicProcess) Running(ctx context.Context) bool {
	// if we haven't created the command or it hasn't started than
	// it isn't running
	if p.cmd == nil || p.cmd.Process == nil {
		return false
	}

	// ProcessState is populated once you start waiting for the
	// process, but not until then. Exited can be false if the
	// process was stopped/canceled.
	if p.cmd.ProcessState != nil && !p.cmd.ProcessState.Exited() {
		return true
	}

	if p.cmd.Process.Pid < 0 {
		return false
	}

	// if we have a viable pid then it's (probably) running
	return true
}

func (p *basicProcess) Signal(ctx context.Context, sig syscall.Signal) error {
	return errors.Wrapf(p.cmd.Process.Signal(sig), "problem sending signal '%s' to '%s'", sig, p.id)
}

func (p *basicProcess) Wait(ctx context.Context) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}

	if p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited() {
		return nil
	}

	sig := make(chan error)
	go func() {
		defer close(sig)

		select {
		case sig <- p.cmd.Wait():
			p.triggers.Run(p.Info(ctx))
		case <-ctx.Done():
			select {
			case sig <- ctx.Err():
			default:
			}
		}

		return
	}()

	select {
	case <-ctx.Done():
		return errors.New("context canceled while waiting for process to exit")
	case err := <-sig:
		return errors.WithStack(err)
	}
}

func (p *basicProcess) RegisterTrigger(trigger ProcessTrigger) error {
	if p.cmd != nil || p.cmd.Process.Pid == -1 {
		return errors.New("cannot register trigger for complete project")
	}

	if trigger == nil {
		return errors.New("cannot register nil trigger")
	}

	p.triggers = append(p.triggers, trigger)

	return nil
}

func (p *basicProcess) Tag(t string) {
	_, ok := p.tags[t]
	if ok {
		return
	}

	p.tags[t] = struct{}{}
	p.opts.Tags = append(p.opts.Tags, t)
}

func (p *basicProcess) ResetTags() {
	p.tags = make(map[string]struct{})
	p.opts.Tags = []string{}
}

func (p *basicProcess) GetTags() []string {
	out := []string{}
	for t := range p.tags {
		out = append(out, t)
	}
	return out
}
