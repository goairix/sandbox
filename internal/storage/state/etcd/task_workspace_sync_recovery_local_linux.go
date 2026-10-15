//go:build linux

package etcd

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func workspaceSyncRecoveryArchiveStamp(f *os.File) ([5]int64, error) {
	var st unix.Stat_t
	err := unix.Fstat(int(f.Fd()), &st)
	return [5]int64{st.Ctim.Sec, st.Ctim.Nsec, st.Mtim.Sec, st.Mtim.Nsec, st.Size}, err
}

func (s *taskWorkspaceSyncRecoverySource) reconcileLocal(ctx context.Context, body *taskWorkspaceSyncRecoveryBody, charge *taskWorkspaceSyncRecoveryCharge) (completion *taskWorkspaceSyncRecoveryCompletion, resultErr error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	defer s.end()
	if err := s.validRecoveryBody(body, charge); err != nil {
		return nil, err
	}
	body.mu.Lock()
	if body.active || body.closed || body.bytes != 0 || body.complete {
		body.mu.Unlock()
		return nil, ErrInvalidRecord
	}
	body.active, body.done = true, make(chan struct{})
	body.mu.Unlock()
	defer func() { body.mu.Lock(); body.active = false; close(body.done); body.mu.Unlock() }()
	if err := s.target.begin(ctx); err != nil {
		return nil, err
	}
	defer s.target.ops.Done()
	if s.fullLocal {
		if s.finalization == nil {
			return nil, ErrInvalidRecord
		}
		if err := s.finalization.repairRegular(ctx, body); err != nil {
			return nil, err
		}
	}
	permission, err := s.prepareLocalPermission(ctx, body)
	if err != nil {
		return nil, err
	}
	defer func() {
		if permission != nil {
			resultErr = errors.Join(resultErr, permission.close())
			if resultErr != nil {
				completion = nil
			}
		}
	}()
	guard := func() error {
		if permission != nil {
			return permission.guard(ctx)
		}
		return s.guard(ctx)
	}
	completed := func(stage string) {
		if s.localIOComplete != nil {
			s.localIOComplete(stage)
		}
	}
	result, err := executeWorkspaceSyncLocalPut(ctx, s.target, body.entry.workspaceSyncLocalFile, body.readOwned, guard, completed)
	if err != nil {
		return nil, err
	}
	if permission != nil {
		joined := permission
		permission = nil
		if err := joined.close(); err != nil {
			return nil, err
		}
		if err := s.guard(ctx); err != nil {
			return nil, err
		}
	}
	if !body.complete || body.bytes != charge.record.Size {
		return nil, ErrInvalidRecord
	}
	q := &taskWorkspaceSyncRecoveryCompletion{source: s, body: body, charge: charge, physical: result, beforeTerminal: charge.generation}
	q.self = q
	body.completion = q
	q.value, err = s.recoveryTerminalValue(q)
	if err != nil {
		return nil, err
	}
	return q, errors.Join(s.guard(ctx), body.ctx.Err())
}
